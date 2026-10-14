package qos

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"
)

type shortWriteConn struct{ net.Conn }

func (c shortWriteConn) Write(p []byte) (int, error) { return len(p) / 2, nil }

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return len(p) / 2, nil }

func TestLimitedConnReportsShortWrites(t *testing.T) {
	lc := NewLimitedConn(context.Background(), shortWriteConn{}, NewLimiter(Config{Bandwidth: 1024, Burst: 1024}))
	n, err := lc.Write([]byte("hello!"))
	if n != 3 || !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("Write = %d, %v; want 3, io.ErrShortWrite", n, err)
	}
}

func TestLimitedConnWriteToReportsShortWrites(t *testing.T) {
	lc := NewLimitedConn(context.Background(), newMockConn([]byte("hello!")), nil)
	n, err := lc.WriteTo(shortWriter{})
	if n != 3 || !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("WriteTo = %d, %v; want 3, io.ErrShortWrite", n, err)
	}
}

func TestLimitedConnCloseInterruptsTokenWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	limiter := NewLimiter(Config{Bandwidth: 1, Burst: 1})
	limiter.RateLimiter().AllowN(time.Now(), 1)
	lc := NewLimitedConn(ctx, newMockConn(nil), limiter)
	done := make(chan error, 1)
	go func() { _, err := lc.Write([]byte("x")); done <- err }()
	time.Sleep(10 * time.Millisecond)
	_ = lc.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Write succeeded after Close")
		}
	case <-time.After(200 * time.Millisecond):
		cancel()
		<-done
		t.Fatal("Close did not interrupt a pending token wait")
	}
}

func TestLimitedConnWriteDeadlineInterruptsTokenWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	limiter := NewLimiter(Config{Bandwidth: 1, Burst: 1})
	limiter.RateLimiter().AllowN(time.Now(), 1)
	lc := NewLimitedConn(ctx, newMockConn(nil), limiter)
	done := make(chan error, 1)
	go func() { _, err := lc.Write([]byte("x")); done <- err }()
	time.Sleep(10 * time.Millisecond)
	_ = lc.SetWriteDeadline(time.Now().Add(10 * time.Millisecond))
	select {
	case err := <-done:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("Write error = %v; want deadline exceeded", err)
		}
	case <-time.After(200 * time.Millisecond):
		cancel()
		<-done
		t.Fatal("SetWriteDeadline did not interrupt a pending token wait")
	}
}
