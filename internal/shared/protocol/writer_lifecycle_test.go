package protocol

import (
	"errors"
	"io"
	"testing"
	"time"
)

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

type shortFrameWriter struct{}

func (shortFrameWriter) Write(p []byte) (int, error) { return len(p) / 2, nil }

func TestWriteFrameRejectsShortWrite(t *testing.T) {
	for _, payload := range [][]byte{nil, []byte("payload")} {
		if err := WriteFrame(shortFrameWriter{}, NewFrame(FrameTypeRegister, payload)); !errors.Is(err, io.ErrShortWrite) {
			t.Fatalf("WriteFrame = %v, want short write", err)
		}
	}
}

func TestFrameWriterStopsAfterWriteError(t *testing.T) {
	fw := NewFrameWriterWithConfig(failingWriter{}, 1, time.Millisecond, 8)
	defer fw.Close()
	if err := fw.WriteFrame(NewFrame(FrameTypeHeartbeat, nil)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fw.done:
	case <-time.After(time.Second):
		t.Fatal("writer did not stop after write failure")
	}
	if err := fw.Close(); err != nil {
		t.Fatal(err)
	}
	if fw.QueuedFrames() != 0 || fw.QueuedBytes() != 0 {
		t.Fatal("backlog was retained after failure")
	}
}

func TestFrameWriterCloseWakesBlockedEnqueue(t *testing.T) {
	fw := &FrameWriter{conn: io.Discard, queue: make(chan *Frame, 1), controlQueue: make(chan *Frame, 1), done: make(chan struct{})}
	if err := fw.WriteFrame(NewFrame(FrameTypeHeartbeat, nil)); err != nil {
		t.Fatal(err)
	}
	written := make(chan error, 1)
	go func() { written <- fw.WriteFrameWithCancel(NewFrame(FrameTypeHeartbeat, nil), make(chan struct{})) }()
	deadline := time.Now().Add(time.Second)
	for fw.QueuedFrames() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	closed := make(chan struct{})
	go func() { _ = fw.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close waited for a blocked enqueue")
	}
	if err := <-written; err == nil {
		t.Fatal("blocked write succeeded after Close")
	}
}
