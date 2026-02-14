package qos

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

func TestNewLimiter(t *testing.T) {
	tests := []struct {
		name      string
		cfg       Config
		wantLimit bool
		wantBurst int
	}{
		{
			name:      "unlimited when bandwidth is 0",
			cfg:       Config{Bandwidth: 0},
			wantLimit: false,
		},
		{
			name:      "limited with default burst (2x)",
			cfg:       Config{Bandwidth: 1024},
			wantLimit: true,
			wantBurst: 2048, // 2x bandwidth
		},
		{
			name:      "limited with custom burst",
			cfg:       Config{Bandwidth: 1024, Burst: 4096},
			wantLimit: true,
			wantBurst: 4096,
		},
		{
			name:      "1MB/s with 2x burst",
			cfg:       Config{Bandwidth: 1024 * 1024},
			wantLimit: true,
			wantBurst: 2 * 1024 * 1024,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := NewLimiter(tt.cfg)
			if l.IsLimited() != tt.wantLimit {
				t.Errorf("IsLimited() = %v, want %v", l.IsLimited(), tt.wantLimit)
			}
			if tt.wantLimit {
				if l.RateLimiter() == nil {
					t.Error("RateLimiter() should not be nil when limited")
				}
				if l.RateLimiter().Burst() != tt.wantBurst {
					t.Errorf("Burst() = %v, want %v", l.RateLimiter().Burst(), tt.wantBurst)
				}
			}
		})
	}
}

func TestLimiterBandwidthEnforcement(t *testing.T) {
	bandwidth := int64(10 * 1024)
	burst := int(bandwidth * 2)

	l := NewLimiter(Config{Bandwidth: bandwidth, Burst: burst})

	if !l.IsLimited() {
		t.Fatal("Limiter should be limited")
	}

	ctx := context.Background()

	start := time.Now()
	err := l.RateLimiter().WaitN(ctx, burst)
	if err != nil {
		t.Fatalf("WaitN failed: %v", err)
	}
	burstDuration := time.Since(start)
	if burstDuration > 100*time.Millisecond {
		t.Errorf("Burst should be instant, took %v", burstDuration)
	}

	start = time.Now()
	err = l.RateLimiter().WaitN(ctx, int(bandwidth)) // Request 1 second worth
	if err != nil {
		t.Fatalf("WaitN failed: %v", err)
	}
	limitedDuration := time.Since(start)

	if limitedDuration < 800*time.Millisecond {
		t.Errorf("Rate limiting not working, took only %v for 1 second worth of data", limitedDuration)
	}
	if limitedDuration > 1500*time.Millisecond {
		t.Errorf("Rate limiting too slow, took %v for 1 second worth of data", limitedDuration)
	}
}

type mockConn struct {
	readBuf  []byte
	readPos  int
	writeBuf []byte
	mu       sync.Mutex
}

func newMockConn(data []byte) *mockConn {
	return &mockConn{readBuf: data}
}

func (c *mockConn) Read(b []byte) (n int, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.readPos >= len(c.readBuf) {
		return 0, io.EOF
	}
	n = copy(b, c.readBuf[c.readPos:])
	c.readPos += n
	return n, nil
}

func (c *mockConn) Write(b []byte) (n int, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writeBuf = append(c.writeBuf, b...)
	return len(b), nil
}

func (c *mockConn) Close() error                       { return nil }
func (c *mockConn) LocalAddr() net.Addr                { return nil }
func (c *mockConn) RemoteAddr() net.Addr               { return nil }
func (c *mockConn) SetDeadline(t time.Time) error      { return nil }
func (c *mockConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *mockConn) SetWriteDeadline(t time.Time) error { return nil }

func TestLimitedConnRead(t *testing.T) {
	dataSize := 20 * 1024
	testData := make([]byte, dataSize)
	for i := range testData {
		testData[i] = byte(i % 256)
	}

	// 10KB/s limit, 20KB burst
	bandwidth := int64(10 * 1024)
	limiter := NewLimiter(Config{Bandwidth: bandwidth, Burst: int(bandwidth * 2)})

	conn := newMockConn(testData)
	ctx := context.Background()
	limitedConn := NewLimitedConn(ctx, conn, limiter)

	buf := make([]byte, dataSize)
	start := time.Now()

	totalRead := 0
	for totalRead < dataSize {
		n, err := limitedConn.Read(buf[totalRead:])
		if err != nil && err != io.EOF {
			t.Fatalf("Read failed: %v", err)
		}
		totalRead += n
		if err == io.EOF {
			break
		}
	}

	duration := time.Since(start)

	if totalRead != dataSize {
		t.Errorf("Read %d bytes, want %d", totalRead, dataSize)
	}

	t.Logf("Read %d bytes in %v", totalRead, duration)
}

func TestLimitedConnWrite(t *testing.T) {
	bandwidth := int64(10 * 1024)
	limiter := NewLimiter(Config{Bandwidth: bandwidth, Burst: int(bandwidth * 2)})

	conn := newMockConn(nil)
	ctx := context.Background()
	limitedConn := NewLimitedConn(ctx, conn, limiter)

	dataSize := 30 * 1024
	testData := make([]byte, dataSize)
	for i := range testData {
		testData[i] = byte(i % 256)
	}

	start := time.Now()

	chunkSize := 10 * 1024
	for i := 0; i < dataSize; i += chunkSize {
		end := i + chunkSize
		if end > dataSize {
			end = dataSize
		}
		n, err := limitedConn.Write(testData[i:end])
		if err != nil {
			t.Fatalf("Write failed: %v", err)
		}
		if n != end-i {
			t.Errorf("Write returned %d, want %d", n, end-i)
		}
	}

	duration := time.Since(start)

	// 30KB data, 10KB/s rate, 20KB burst → ~1s for remaining 10KB
	if duration < 800*time.Millisecond {
		t.Errorf("Write too fast, took %v for 30KB with 10KB/s limit and 20KB burst", duration)
	}

	t.Logf("Wrote %d bytes in %v", dataSize, duration)
}

func TestLimitedConnUnlimited(t *testing.T) {
	limiter := NewLimiter(Config{Bandwidth: 0})

	testData := make([]byte, 100*1024)
	conn := newMockConn(testData)
	ctx := context.Background()
	limitedConn := NewLimitedConn(ctx, conn, limiter)

	buf := make([]byte, len(testData))
	start := time.Now()

	totalRead := 0
	for totalRead < len(testData) {
		n, err := limitedConn.Read(buf[totalRead:])
		if err != nil && err != io.EOF {
			t.Fatalf("Read failed: %v", err)
		}
		totalRead += n
		if err == io.EOF {
			break
		}
	}

	duration := time.Since(start)

	if totalRead != len(testData) {
		t.Errorf("Read %d bytes, want %d", totalRead, len(testData))
	}

	if duration > 100*time.Millisecond {
		t.Errorf("Unlimited read took too long: %v", duration)
	}
}

func TestLimitedConnContextCancellation(t *testing.T) {
	bandwidth := int64(100)
	limiter := NewLimiter(Config{Bandwidth: bandwidth, Burst: 100})

	conn := newMockConn(nil)
	ctx, cancel := context.WithCancel(context.Background())
	limitedConn := NewLimitedConn(ctx, conn, limiter)

	_, err := limitedConn.Write(make([]byte, 100))
	if err != nil {
		t.Fatalf("First write failed: %v", err)
	}

	cancel()

	_, err = limitedConn.Write(make([]byte, 1000))
	if err == nil {
		t.Error("Write should fail after context cancellation")
	}
}

func TestBurstMultiplier(t *testing.T) {
	tests := []struct {
		name       string
		bandwidth  int64
		multiplier float64
		wantBurst  int
	}{
		{
			name:       "2x multiplier",
			bandwidth:  1024 * 1024, // 1MB/s
			multiplier: 2.0,
			wantBurst:  2 * 1024 * 1024,
		},
		{
			name:       "2.5x multiplier",
			bandwidth:  1024 * 1024,
			multiplier: 2.5,
			wantBurst:  int(float64(1024*1024) * 2.5),
		},
		{
			name:       "1x multiplier (no extra burst)",
			bandwidth:  1024 * 1024,
			multiplier: 1.0,
			wantBurst:  1024 * 1024,
		},
		{
			name:       "3x multiplier",
			bandwidth:  500 * 1024, // 500KB/s
			multiplier: 3.0,
			wantBurst:  3 * 500 * 1024,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			burst := int(float64(tt.bandwidth) * tt.multiplier)
			l := NewLimiter(Config{Bandwidth: tt.bandwidth, Burst: burst})

			if l.RateLimiter().Burst() != tt.wantBurst {
				t.Errorf("Burst() = %v, want %v", l.RateLimiter().Burst(), tt.wantBurst)
			}
		})
	}
}
