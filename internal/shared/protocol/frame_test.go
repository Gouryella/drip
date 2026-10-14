package protocol

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

func TestWriteReadFrameMaxPayloadBoundary(t *testing.T) {
	payload := bytes.Repeat([]byte{0xAB}, MaxFrameSize)
	var buf bytes.Buffer

	if err := WriteFrame(&buf, NewFrame(FrameTypeDataConnect, payload)); err != nil {
		t.Fatalf("WriteFrame() failed at MaxFrameSize: %v", err)
	}

	frame, err := ReadFrame(&buf)
	if err != nil {
		t.Fatalf("ReadFrame() failed at MaxFrameSize: %v", err)
	}
	defer frame.Release()

	if frame.Type != FrameTypeDataConnect {
		t.Fatalf("frame type = %v, want %v", frame.Type, FrameTypeDataConnect)
	}
	if !bytes.Equal(frame.Payload, payload) {
		t.Fatal("payload did not round-trip at MaxFrameSize")
	}
}

func TestWriteFrameRejectsPayloadLargerThanMax(t *testing.T) {
	payload := bytes.Repeat([]byte{0xCD}, MaxFrameSize+1)
	var buf bytes.Buffer

	err := WriteFrame(&buf, NewFrame(FrameTypeDataConnect, payload))
	if err == nil {
		t.Fatal("WriteFrame() succeeded for oversized payload")
	}
	if !strings.Contains(err.Error(), "payload too large") {
		t.Fatalf("WriteFrame() error = %v, want payload too large", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("WriteFrame() wrote %d bytes for rejected frame", buf.Len())
	}
}

func TestReadFrameRejectsPayloadLargerThanMax(t *testing.T) {
	var header [FrameHeaderSize]byte
	binary.BigEndian.PutUint32(header[0:4], uint32(MaxFrameSize+1))
	header[4] = byte(FrameTypeDataConnect)

	_, err := ReadFrame(bytes.NewReader(header[:]))
	if err == nil {
		t.Fatal("ReadFrame() succeeded for oversized payload length")
	}
	if !strings.Contains(err.Error(), "payload too large") {
		t.Fatalf("ReadFrame() error = %v, want payload too large", err)
	}
}

func TestReadFrameRejectsInvalidFrameType(t *testing.T) {
	var header [FrameHeaderSize]byte
	header[4] = 0xFF

	_, err := ReadFrame(bytes.NewReader(header[:]))
	if err == nil {
		t.Fatal("ReadFrame() succeeded for invalid frame type")
	}
	if !strings.Contains(err.Error(), "invalid frame type") {
		t.Fatalf("ReadFrame() error = %v, want invalid frame type", err)
	}
}

func TestReadFrameShortPayloadReturnsError(t *testing.T) {
	var buf bytes.Buffer
	var header [FrameHeaderSize]byte
	binary.BigEndian.PutUint32(header[0:4], 4)
	header[4] = byte(FrameTypeDataConnect)
	buf.Write(header[:])
	buf.Write([]byte{1, 2})

	_, err := ReadFrame(&buf)
	if err == nil {
		t.Fatal("ReadFrame() succeeded with truncated payload")
	}
	if !strings.Contains(err.Error(), "failed to read payload") {
		t.Fatalf("ReadFrame() error = %v, want failed to read payload", err)
	}
}
