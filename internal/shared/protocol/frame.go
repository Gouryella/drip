package protocol

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"

	"drip/internal/shared/pool"
)

const (
	FrameHeaderSize = 5
	// MaxFrameSize limits payload size to prevent memory exhaustion attacks.
	// 1MB is sufficient for most HTTP requests/responses while limiting DoS impact.
	MaxFrameSize = 1 * 1024 * 1024 // 1MB (reduced from 10MB)
)

// FrameType defines the type of frame
type FrameType byte

const (
	FrameTypeRegister       FrameType = 0x01
	FrameTypeRegisterAck    FrameType = 0x02
	FrameTypeHeartbeat      FrameType = 0x03
	FrameTypeHeartbeatAck   FrameType = 0x04
	FrameTypeClose          FrameType = 0x05
	FrameTypeError          FrameType = 0x06
	FrameTypeDataConnect    FrameType = 0x07
	FrameTypeDataConnectAck FrameType = 0x08
)

// String returns the string representation of frame type
func (t FrameType) String() string {
	switch t {
	case FrameTypeRegister:
		return "Register"
	case FrameTypeRegisterAck:
		return "RegisterAck"
	case FrameTypeHeartbeat:
		return "Heartbeat"
	case FrameTypeHeartbeatAck:
		return "HeartbeatAck"
	case FrameTypeClose:
		return "Close"
	case FrameTypeError:
		return "Error"
	case FrameTypeDataConnect:
		return "DataConnect"
	case FrameTypeDataConnectAck:
		return "DataConnectAck"
	default:
		return fmt.Sprintf("Unknown(%d)", t)
	}
}

type Frame struct {
	Type       FrameType
	Payload    []byte
	poolBuffer *[]byte
	// queuedBytes is set by FrameWriter when the frame is enqueued.
	// It allows the writer to decrement backlog counters exactly once.
	queuedBytes int64
}

func WriteFrame(w io.Writer, frame *Frame) error {
	payloadLen := len(frame.Payload)
	if payloadLen > MaxFrameSize {
		return fmt.Errorf("payload too large: %d bytes (max %d)", payloadLen, MaxFrameSize)
	}

	var header [FrameHeaderSize]byte
	binary.BigEndian.PutUint32(header[0:4], uint32(payloadLen))
	header[4] = byte(frame.Type)

	if payloadLen == 0 {
		if _, err := w.Write(header[:]); err != nil {
			return fmt.Errorf("failed to write frame header: %w", err)
		}
		return nil
	}

	// net.Buffers will use writev for TCP connections and falls back to
	// sequential writes for other io.Writer implementations (e.g. TLS).
	if _, err := (&net.Buffers{header[:], frame.Payload}).WriteTo(w); err != nil {
		return fmt.Errorf("failed to write frame: %w", err)
	}

	return nil
}

func ReadFrame(r io.Reader) (*Frame, error) {
	// Use stack-allocated array to avoid heap allocation
	var header [FrameHeaderSize]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, fmt.Errorf("failed to read frame header: %w", err)
	}

	payloadLen := binary.BigEndian.Uint32(header[0:4])
	if payloadLen > MaxFrameSize {
		return nil, fmt.Errorf("payload too large: %d bytes (max %d)", payloadLen, MaxFrameSize)
	}

	frameType := FrameType(header[4])

	var payload []byte
	var poolBuf *[]byte

	if payloadLen > 0 {
		if payloadLen > pool.SizeLarge {
			payload = make([]byte, payloadLen)
			if _, err := io.ReadFull(r, payload); err != nil {
				return nil, fmt.Errorf("failed to read payload: %w", err)
			}
		} else {
			poolBuf = pool.GetBuffer(int(payloadLen))
			payload = (*poolBuf)[:payloadLen]

			if _, err := io.ReadFull(r, payload); err != nil {
				pool.PutBuffer(poolBuf)
				return nil, fmt.Errorf("failed to read payload: %w", err)
			}
		}
	}

	return &Frame{
		Type:       frameType,
		Payload:    payload,
		poolBuffer: poolBuf,
	}, nil
}

func (f *Frame) Release() {
	if f.poolBuffer != nil {
		pool.PutBuffer(f.poolBuffer)
		f.poolBuffer = nil
		f.Payload = nil
	}
	// Reset queued marker to avoid carrying over stale state if the frame is reused.
	f.queuedBytes = 0
}

// NewFrame creates a new frame
func NewFrame(frameType FrameType, payload []byte) *Frame {
	return &Frame{
		Type:    frameType,
		Payload: payload,
	}
}

// NewFramePooled creates a new frame with a pooled buffer
// The poolBuffer will be automatically released after the frame is written
func NewFramePooled(frameType FrameType, payload []byte, poolBuffer *[]byte) *Frame {
	return &Frame{
		Type:       frameType,
		Payload:    payload,
		poolBuffer: poolBuffer,
	}
}
