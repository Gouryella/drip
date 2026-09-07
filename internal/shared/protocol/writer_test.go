package protocol

import (
	"io"
	"strings"
	"testing"
	"time"
)

func TestFrameWriterWriteAfterCloseReturnsError(t *testing.T) {
	fw := NewFrameWriterWithConfig(io.Discard, 1, time.Hour, 1)
	if err := fw.Close(); err != nil {
		t.Fatalf("Close() failed: %v", err)
	}

	err := fw.WriteFrame(NewFrame(FrameTypeHeartbeat, nil))
	if err == nil {
		t.Fatal("WriteFrame() after Close succeeded")
	}
	if !strings.Contains(err.Error(), "writer closed") {
		t.Fatalf("WriteFrame() after Close error = %v, want writer closed", err)
	}
}

func TestFrameWriterFullQueueCancelDoesNotLeakBacklog(t *testing.T) {
	fw := &FrameWriter{
		conn:         io.Discard,
		queue:        make(chan *Frame, 1),
		controlQueue: make(chan *Frame, 1),
		done:         make(chan struct{}),
	}
	fw.queue <- NewFrame(FrameTypeDataConnect, []byte("already queued"))

	queuedBeforeCancel := fw.QueuedFrames()
	cancel := make(chan struct{})
	close(cancel)

	err := fw.WriteFrameWithCancel(NewFrame(FrameTypeDataConnect, []byte("third")), cancel)
	if err == nil {
		t.Fatal("WriteFrameWithCancel() succeeded while queue was full and cancel was closed")
	}
	if !strings.Contains(err.Error(), "write cancelled") {
		t.Fatalf("WriteFrameWithCancel() error = %v, want write cancelled", err)
	}
	if got := fw.QueuedFrames(); got != queuedBeforeCancel {
		t.Fatalf("QueuedFrames() = %d after cancelled write, want %d", got, queuedBeforeCancel)
	}
}
