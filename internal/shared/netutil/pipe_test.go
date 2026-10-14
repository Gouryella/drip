package netutil

import (
	"errors"
	"net"
	"testing"
	"time"
)

func TestCopyBufferWriteDeadlineReleasesSlowReader(t *testing.T) {
	srcReader, srcWriter := net.Pipe()
	dstReader, dstWriter := net.Pipe()
	defer srcReader.Close()
	defer srcWriter.Close()
	defer dstReader.Close()
	defer dstWriter.Close()

	writeDone := make(chan error, 1)
	go func() {
		_, err := srcWriter.Write([]byte("x"))
		writeDone <- err
	}()

	start := time.Now()
	_, err := copyBuffer(dstWriter, srcReader, make([]byte, 1), nil, make(chan struct{}), 20*time.Millisecond)
	if err == nil {
		t.Fatal("copyBuffer() succeeded with a destination that never reads")
	}

	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("copyBuffer() error = %v, want timeout", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("copyBuffer() took %v, want bounded by write deadline", elapsed)
	}

	select {
	case <-writeDone:
	case <-time.After(time.Second):
		t.Fatal("source writer remained blocked after copyBuffer returned")
	}
}
