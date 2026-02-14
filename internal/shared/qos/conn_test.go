package qos

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

type errorAfterConn struct {
	mockConn
	writeLimit int
	written    int
}

func (c *errorAfterConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	remaining := c.writeLimit - c.written
	if remaining <= 0 {
		return 0, errors.New("write error")
	}
	if len(b) > remaining {
		b = b[:remaining]
	}
	c.writeBuf = append(c.writeBuf, b...)
	c.written += len(b)
	return len(b), nil
}

func TestWriteLargerThanBurst(t *testing.T) {
	// 10KB/s, burst=1KB — write 5KB should be chunked into 5 pieces
	bandwidth := int64(10 * 1024)
	burst := 1024
	limiter := NewLimiter(Config{Bandwidth: bandwidth, Burst: burst})

	conn := newMockConn(nil)
	lc := NewLimitedConn(context.Background(), conn, limiter)

	data := make([]byte, 5*1024)
	for i := range data {
		data[i] = byte(i % 256)
	}

	n, err := lc.Write(data)
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	if n != len(data) {
		t.Errorf("Write returned %d, want %d", n, len(data))
	}

	conn.mu.Lock()
	defer conn.mu.Unlock()
	if !bytes.Equal(conn.writeBuf, data) {
		t.Error("Written data does not match input")
	}
}

func TestWriteExactBurstSize(t *testing.T) {
	burst := 2048
	limiter := NewLimiter(Config{Bandwidth: 10240, Burst: burst})

	conn := newMockConn(nil)
	lc := NewLimitedConn(context.Background(), conn, limiter)

	data := make([]byte, burst)
	start := time.Now()
	n, err := lc.Write(data)
	dur := time.Since(start)

	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	if n != burst {
		t.Errorf("Write returned %d, want %d", n, burst)
	}
	// Exact burst should be instant
	if dur > 100*time.Millisecond {
		t.Errorf("Exact burst write should be instant, took %v", dur)
	}
}

func TestWriteZeroLength(t *testing.T) {
	limiter := NewLimiter(Config{Bandwidth: 1024, Burst: 1024})
	conn := newMockConn(nil)
	lc := NewLimitedConn(context.Background(), conn, limiter)

	n, err := lc.Write(nil)
	if err != nil {
		t.Fatalf("Write(nil) failed: %v", err)
	}
	if n != 0 {
		t.Errorf("Write(nil) returned %d, want 0", n)
	}

	n, err = lc.Write([]byte{})
	if err != nil {
		t.Fatalf("Write([]) failed: %v", err)
	}
	if n != 0 {
		t.Errorf("Write([]) returned %d, want 0", n)
	}
}

func TestWriteContextCancelDuringChunking(t *testing.T) {
	// Very slow rate, small burst so second chunk must wait
	limiter := NewLimiter(Config{Bandwidth: 100, Burst: 100})
	conn := newMockConn(nil)
	ctx, cancel := context.WithCancel(context.Background())
	lc := NewLimitedConn(ctx, conn, limiter)

	// Use up burst
	_, err := lc.Write(make([]byte, 100))
	if err != nil {
		t.Fatalf("First write failed: %v", err)
	}

	cancel()

	// This write needs more tokens but context is cancelled
	_, err = lc.Write(make([]byte, 200))
	if err == nil {
		t.Error("Write should fail after context cancellation")
	}
}

func TestWritePartialErrorMidChunk(t *testing.T) {
	// Underlying conn fails after 500 bytes
	conn := &errorAfterConn{writeLimit: 500}
	limiter := NewLimiter(Config{Bandwidth: 100000, Burst: 1024})
	lc := NewLimitedConn(context.Background(), conn, limiter)

	data := make([]byte, 2048)
	n, err := lc.Write(data)
	if err == nil {
		t.Error("Expected write error")
	}
	if n < 500 {
		t.Errorf("Expected at least 500 bytes written, got %d", n)
	}
	if n > 1024 {
		t.Errorf("Expected at most 1024 bytes written (one chunk), got %d", n)
	}
}

func TestReadCappedToBurst(t *testing.T) {
	burst := 512
	limiter := NewLimiter(Config{Bandwidth: 10240, Burst: burst})

	// Provide 4KB of data
	data := make([]byte, 4096)
	for i := range data {
		data[i] = byte(i % 256)
	}
	conn := newMockConn(data)
	lc := NewLimitedConn(context.Background(), conn, limiter)

	// Request 4KB read, should get at most burst (512) bytes
	buf := make([]byte, 4096)
	n, err := lc.Read(buf)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}
	if n > burst {
		t.Errorf("Read returned %d bytes, should be capped at burst=%d", n, burst)
	}
	if !bytes.Equal(buf[:n], data[:n]) {
		t.Error("Read data mismatch")
	}
}

func TestReadSmallerThanBurst(t *testing.T) {
	burst := 4096
	limiter := NewLimiter(Config{Bandwidth: 10240, Burst: burst})

	data := make([]byte, 100)
	conn := newMockConn(data)
	lc := NewLimitedConn(context.Background(), conn, limiter)

	buf := make([]byte, 100)
	n, err := lc.Read(buf)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}
	if n != 100 {
		t.Errorf("Read returned %d, want 100", n)
	}
}

func TestReadEOF(t *testing.T) {
	limiter := NewLimiter(Config{Bandwidth: 1024, Burst: 1024})
	conn := newMockConn([]byte{}) // empty
	lc := NewLimitedConn(context.Background(), conn, limiter)

	buf := make([]byte, 100)
	_, err := lc.Read(buf)
	if err != io.EOF {
		t.Errorf("Expected io.EOF, got %v", err)
	}
}

func TestReadContextCancel(t *testing.T) {
	// Slow rate, small burst
	limiter := NewLimiter(Config{Bandwidth: 100, Burst: 100})
	data := make([]byte, 200)
	conn := newMockConn(data)
	ctx, cancel := context.WithCancel(context.Background())
	lc := NewLimitedConn(ctx, conn, limiter)

	// First read uses burst
	buf := make([]byte, 100)
	_, err := lc.Read(buf)
	if err != nil {
		t.Fatalf("First read failed: %v", err)
	}

	cancel()

	// Second read should fail on WaitN
	_, err = lc.Read(buf)
	if err == nil {
		t.Error("Read should fail after context cancellation")
	}
}

func TestReadFromInterface(t *testing.T) {
	limiter := NewLimiter(Config{Bandwidth: 1024 * 1024, Burst: 1024 * 1024})
	conn := newMockConn(nil)
	lc := NewLimitedConn(context.Background(), conn, limiter)

	var _ io.ReaderFrom = lc
}

func TestWriteToInterface(t *testing.T) {
	limiter := NewLimiter(Config{Bandwidth: 1024 * 1024, Burst: 1024 * 1024})
	data := make([]byte, 100)
	conn := newMockConn(data)
	lc := NewLimitedConn(context.Background(), conn, limiter)

	var _ io.WriterTo = lc
}

func TestReadFromBasic(t *testing.T) {
	limiter := NewLimiter(Config{Bandwidth: 1024 * 1024, Burst: 1024 * 1024})
	conn := newMockConn(nil)
	lc := NewLimitedConn(context.Background(), conn, limiter)

	src := bytes.NewReader(make([]byte, 50*1024))
	n, err := lc.ReadFrom(src)
	if err != nil {
		t.Fatalf("ReadFrom failed: %v", err)
	}
	if n != 50*1024 {
		t.Errorf("ReadFrom transferred %d bytes, want %d", n, 50*1024)
	}

	conn.mu.Lock()
	defer conn.mu.Unlock()
	if len(conn.writeBuf) != 50*1024 {
		t.Errorf("Underlying conn received %d bytes, want %d", len(conn.writeBuf), 50*1024)
	}
}

func TestReadFromReaderError(t *testing.T) {
	limiter := NewLimiter(Config{Bandwidth: 1024 * 1024, Burst: 1024 * 1024})
	conn := newMockConn(nil)
	lc := NewLimitedConn(context.Background(), conn, limiter)

	errReader := &failingReader{failAfter: 100}
	n, err := lc.ReadFrom(errReader)
	if err == nil {
		t.Error("Expected error from ReadFrom")
	}
	if n != 100 {
		t.Errorf("ReadFrom transferred %d bytes before error, want 100", n)
	}
}

func TestWriteToBasic(t *testing.T) {
	data := make([]byte, 50*1024)
	for i := range data {
		data[i] = byte(i % 256)
	}
	limiter := NewLimiter(Config{Bandwidth: 1024 * 1024, Burst: 1024 * 1024})
	conn := newMockConn(data)
	lc := NewLimitedConn(context.Background(), conn, limiter)

	var buf bytes.Buffer
	n, err := lc.WriteTo(&buf)
	if err != nil {
		t.Fatalf("WriteTo failed: %v", err)
	}
	if n != int64(len(data)) {
		t.Errorf("WriteTo transferred %d bytes, want %d", n, len(data))
	}
	if !bytes.Equal(buf.Bytes(), data) {
		t.Error("WriteTo data mismatch")
	}
}

func TestWriteToWriterError(t *testing.T) {
	data := make([]byte, 1024)
	limiter := NewLimiter(Config{Bandwidth: 1024 * 1024, Burst: 1024 * 1024})
	conn := newMockConn(data)
	lc := NewLimitedConn(context.Background(), conn, limiter)

	fw := &failingWriter{failAfter: 100}
	_, err := lc.WriteTo(fw)
	if err == nil {
		t.Error("Expected error from WriteTo")
	}
}

func TestReadFromRateLimited(t *testing.T) {
	// 10KB/s, 10KB burst — 20KB transfer should take ~1s
	limiter := NewLimiter(Config{Bandwidth: 10 * 1024, Burst: 10 * 1024})
	conn := newMockConn(nil)
	lc := NewLimitedConn(context.Background(), conn, limiter)

	src := bytes.NewReader(make([]byte, 20*1024))
	start := time.Now()
	n, err := lc.ReadFrom(src)
	dur := time.Since(start)

	if err != nil {
		t.Fatalf("ReadFrom failed: %v", err)
	}
	if n != 20*1024 {
		t.Errorf("ReadFrom transferred %d, want %d", n, 20*1024)
	}
	if dur < 800*time.Millisecond {
		t.Errorf("ReadFrom too fast: %v (expected ~1s for 20KB at 10KB/s with 10KB burst)", dur)
	}
}

// TestIoCopyUsesReadFrom verifies io.Copy goes through our ReadFrom,
// not the underlying conn's optimized path.
func TestIoCopyUsesReadFrom(t *testing.T) {
	// Use a small burst so we can detect if rate limiting is applied
	limiter := NewLimiter(Config{Bandwidth: 10 * 1024, Burst: 10 * 1024})
	conn := newMockConn(nil)
	lc := NewLimitedConn(context.Background(), conn, limiter)

	src := bytes.NewReader(make([]byte, 20*1024))
	start := time.Now()
	n, err := io.Copy(lc, src)
	dur := time.Since(start)

	if err != nil {
		t.Fatalf("io.Copy failed: %v", err)
	}
	if n != 20*1024 {
		t.Errorf("io.Copy transferred %d, want %d", n, 20*1024)
	}
	// If io.Copy bypassed our ReadFrom, it would be instant
	if dur < 800*time.Millisecond {
		t.Errorf("io.Copy too fast (%v), rate limiting may be bypassed", dur)
	}
}

func TestUnlimitedWrite(t *testing.T) {
	limiter := NewLimiter(Config{Bandwidth: 0})
	conn := newMockConn(nil)
	lc := NewLimitedConn(context.Background(), conn, limiter)

	data := make([]byte, 1024*1024) // 1MB
	start := time.Now()
	n, err := lc.Write(data)
	dur := time.Since(start)

	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	if n != len(data) {
		t.Errorf("Write returned %d, want %d", n, len(data))
	}
	if dur > 50*time.Millisecond {
		t.Errorf("Unlimited write took too long: %v", dur)
	}
}

func TestNilLimiter(t *testing.T) {
	conn := newMockConn([]byte("hello"))
	lc := NewLimitedConn(context.Background(), conn, nil)

	buf := make([]byte, 10)
	n, err := lc.Read(buf)
	if err != nil {
		t.Fatalf("Read with nil limiter failed: %v", err)
	}
	if n != 5 {
		t.Errorf("Read returned %d, want 5", n)
	}

	n, err = lc.Write([]byte("world"))
	if err != nil {
		t.Fatalf("Write with nil limiter failed: %v", err)
	}
	if n != 5 {
		t.Errorf("Write returned %d, want 5", n)
	}
}

func TestConcurrentReadWrite(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	limiter := NewLimiter(Config{Bandwidth: 100 * 1024, Burst: 100 * 1024})
	lc := NewLimitedConn(context.Background(), serverConn, limiter)

	dataSize := 50 * 1024
	var wg sync.WaitGroup

	// Writer
	wg.Add(1)
	go func() {
		defer wg.Done()
		data := make([]byte, dataSize)
		for i := range data {
			data[i] = 0xAA
		}
		lc.Write(data)
	}()

	// Reader on the other end
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, dataSize)
		total := 0
		for total < dataSize {
			n, err := clientConn.Read(buf[total:])
			if err != nil {
				return
			}
			total += n
		}
	}()

	wg.Wait()
}

type failingReader struct {
	failAfter int
	read      int
}

func (r *failingReader) Read(b []byte) (int, error) {
	remaining := r.failAfter - r.read
	if remaining <= 0 {
		return 0, errors.New("reader error")
	}
	n := len(b)
	if n > remaining {
		n = remaining
	}
	r.read += n
	return n, nil
}

type failingWriter struct {
	failAfter int
	written   int
}

func (w *failingWriter) Write(b []byte) (int, error) {
	remaining := w.failAfter - w.written
	if remaining <= 0 {
		return 0, errors.New("writer error")
	}
	n := len(b)
	if n > remaining {
		w.written += remaining
		return remaining, errors.New("writer error")
	}
	w.written += n
	return n, nil
}
