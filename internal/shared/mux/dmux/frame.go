// Package dmux implements drip's own stream multiplexer.
//
// The wire format is deliberately small. Every frame has a fixed 12-byte
// header:
//
//	 0        1        2        3        4        5        6        7
//	+--------+--------+--------+--------+--------+--------+--------+--------+
//	| ver(1) | type(1)| flags(2)         | stream id(4)                     |
//	+--------+--------+--------+--------+--------+--------+--------+--------+
//	| payload length(4)                                                     |
//	+--------+--------+--------+--------+--------+--------+--------+--------+
//
// All integers are big-endian.
//
// Streams are opened optimistically: the first DATA frame for a stream carries
// the SYN flag, so opening a stream costs no round trip. A receiver creates
// the stream when it sees SYN; data that arrives for an unknown stream without
// SYN is rejected with RST.
//
// Flow control is per stream and window based. The receiver advertises a fixed
// initial window (identical on both peers) and replenishes it with
// WINDOW_UPDATE frames as the application consumes bytes. There is no
// connection-level window: the bounded send queue provides backpressure and
// bounds memory.
//
// The frame types are:
//
//	DATA           stream payload, SYN/FIN flags
//	WINDOW_UPDATE  4-byte credit increment for a stream
//	GOAWAY         no new streams will be accepted
//	PING/PONG      keepalive with an opaque 8-byte token
//	RESET          abort a stream
package dmux

import (
	"encoding/binary"
	"fmt"
)

const (
	// headerSize is the fixed frame header size.
	headerSize = 12
	// protocolVersion is bumped on incompatible wire changes.
	protocolVersion = 1
	// maxFramePayload caps a single frame's payload. It is also the largest
	// single Write chunk.
	maxFramePayload = 32 * 1024
	// initialWindow is the per-stream receive window both peers assume.
	initialWindow = 256 * 1024
	// maxWindow caps a stream window after WINDOW_UPDATE accounting.
	maxWindow = 4 * 1024 * 1024
	// batchSize is the per-session send buffer size.
	batchSize = 64 * 1024
	// outQueueDepth bounds queued batches per session.
	outQueueDepth = 8
	// acceptBacklog bounds streams waiting for Accept.
	acceptBacklog = 4096
)

type frameType uint8

const (
	frameData frameType = iota + 1
	frameWindowUpdate
	frameGoAway
	framePing
	framePong
	frameReset
)

const (
	// flagSYN marks the first frame of a locally opened stream.
	flagSYN uint16 = 1 << 0
	// flagFIN marks the end of the sender's data for a stream.
	flagFIN uint16 = 1 << 1
)

type frameHeader struct {
	typ      frameType
	flags    uint16
	streamID uint32
	length   uint32
}

func (h frameHeader) encode(dst []byte) {
	_ = dst[headerSize-1] // bounds check hint
	dst[0] = protocolVersion
	dst[1] = byte(h.typ)
	binary.BigEndian.PutUint16(dst[2:4], h.flags)
	binary.BigEndian.PutUint32(dst[4:8], h.streamID)
	binary.BigEndian.PutUint32(dst[8:12], h.length)
}

func decodeHeader(src []byte) (frameHeader, error) {
	_ = src[headerSize-1]
	if src[0] != protocolVersion {
		return frameHeader{}, fmt.Errorf("%w: unsupported version %d", ErrProtocol, src[0])
	}
	h := frameHeader{
		typ:      frameType(src[1]),
		flags:    binary.BigEndian.Uint16(src[2:4]),
		streamID: binary.BigEndian.Uint32(src[4:8]),
		length:   binary.BigEndian.Uint32(src[8:12]),
	}
	switch h.typ {
	case frameData, frameWindowUpdate, frameGoAway, framePing, framePong, frameReset:
	default:
		return frameHeader{}, fmt.Errorf("%w: unknown frame type %d", ErrProtocol, src[1])
	}
	if h.length > maxFramePayload {
		return frameHeader{}, fmt.Errorf("%w: frame payload %d exceeds %d", ErrProtocol, h.length, maxFramePayload)
	}
	switch h.typ {
	case frameWindowUpdate:
		if h.length != 4 {
			return frameHeader{}, fmt.Errorf("%w: window update length %d", ErrProtocol, h.length)
		}
	case framePing, framePong:
		if h.length != 8 {
			return frameHeader{}, fmt.Errorf("%w: ping length %d", ErrProtocol, h.length)
		}
	case frameGoAway, frameReset:
		// These carry a small fixed payload; bound it so a malformed peer
		// cannot force a large allocation on a control frame.
		if h.length > 8 {
			return frameHeader{}, fmt.Errorf("%w: control frame length %d", ErrProtocol, h.length)
		}
	}
	return h, nil
}
