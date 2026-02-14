package qos

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

func TestEndToEndBandwidthLimiting(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	bandwidth := int64(100 * 1024)
	burstMultiplier := 2.0
	burst := int(float64(bandwidth) * burstMultiplier)

	limiter := NewLimiter(Config{Bandwidth: bandwidth, Burst: burst})
	ctx := context.Background()
	limitedServerConn := NewLimitedConn(ctx, serverConn, limiter)

	dataSize := 500 * 1024
	testData := make([]byte, dataSize)
	for i := range testData {
		testData[i] = byte(i % 256)
	}

	var wg sync.WaitGroup
	var writeErr, readErr error
	var writeDuration time.Duration
	receivedData := make([]byte, dataSize)

	wg.Add(1)
	go func() {
		defer wg.Done()
		start := time.Now()
		chunkSize := 32 * 1024
		for i := 0; i < dataSize; i += chunkSize {
			end := i + chunkSize
			if end > dataSize {
				end = dataSize
			}
			_, err := limitedServerConn.Write(testData[i:end])
			if err != nil {
				writeErr = err
				return
			}
		}
		writeDuration = time.Since(start)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		totalRead := 0
		for totalRead < dataSize {
			n, err := clientConn.Read(receivedData[totalRead:])
			if err != nil {
				if err != io.EOF {
					readErr = err
				}
				return
			}
			totalRead += n
		}
	}()

	wg.Wait()

	if writeErr != nil {
		t.Fatalf("Write error: %v", writeErr)
	}
	if readErr != nil {
		t.Fatalf("Read error: %v", readErr)
	}

	for i := 0; i < dataSize; i++ {
		if receivedData[i] != testData[i] {
			t.Fatalf("Data mismatch at byte %d: got %d, want %d", i, receivedData[i], testData[i])
		}
	}

	expectedMinDuration := 2500 * time.Millisecond
	expectedMaxDuration := 4000 * time.Millisecond

	if writeDuration < expectedMinDuration {
		t.Errorf("Transfer too fast: %v (expected >= %v)", writeDuration, expectedMinDuration)
	}
	if writeDuration > expectedMaxDuration {
		t.Errorf("Transfer too slow: %v (expected <= %v)", writeDuration, expectedMaxDuration)
	}

	t.Logf("Transferred %d bytes in %v (rate: %.2f KB/s)",
		dataSize, writeDuration, float64(dataSize)/writeDuration.Seconds()/1024)
}

func TestBidirectionalBandwidthLimiting(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	bandwidth := int64(50 * 1024)
	burst := int(bandwidth * 2)

	serverLimiter := NewLimiter(Config{Bandwidth: bandwidth, Burst: burst})
	clientLimiter := NewLimiter(Config{Bandwidth: bandwidth, Burst: burst})

	ctx := context.Background()
	limitedServerConn := NewLimitedConn(ctx, serverConn, serverLimiter)
	limitedClientConn := NewLimitedConn(ctx, clientConn, clientLimiter)

	dataSize := 200 * 1024
	serverData := make([]byte, dataSize)
	clientData := make([]byte, dataSize)
	for i := range serverData {
		serverData[i] = byte(i % 256)
		clientData[i] = byte((i + 128) % 256)
	}

	var wg sync.WaitGroup
	receivedByClient := make([]byte, dataSize)
	receivedByServer := make([]byte, dataSize)

	// Server writes to client
	wg.Add(1)
	go func() {
		defer wg.Done()
		chunkSize := 16 * 1024
		for i := 0; i < dataSize; i += chunkSize {
			end := i + chunkSize
			if end > dataSize {
				end = dataSize
			}
			limitedServerConn.Write(serverData[i:end])
		}
	}()

	// Client writes to server
	wg.Add(1)
	go func() {
		defer wg.Done()
		chunkSize := 16 * 1024
		for i := 0; i < dataSize; i += chunkSize {
			end := i + chunkSize
			if end > dataSize {
				end = dataSize
			}
			limitedClientConn.Write(clientData[i:end])
		}
	}()

	// Client reads from server
	wg.Add(1)
	go func() {
		defer wg.Done()
		totalRead := 0
		for totalRead < dataSize {
			n, err := limitedClientConn.Read(receivedByClient[totalRead:])
			if err != nil {
				return
			}
			totalRead += n
		}
	}()

	// Server reads from client
	wg.Add(1)
	go func() {
		defer wg.Done()
		totalRead := 0
		for totalRead < dataSize {
			n, err := limitedServerConn.Read(receivedByServer[totalRead:])
			if err != nil {
				return
			}
			totalRead += n
		}
	}()

	wg.Wait()

	for i := 0; i < dataSize; i++ {
		if receivedByClient[i] != serverData[i] {
			t.Fatalf("Client received wrong data at byte %d", i)
		}
		if receivedByServer[i] != clientData[i] {
			t.Fatalf("Server received wrong data at byte %d", i)
		}
	}

	t.Log("Bidirectional transfer completed successfully")
}

func TestBurstBehavior(t *testing.T) {
	bandwidth := int64(10 * 1024)
	burst := 50 * 1024

	limiter := NewLimiter(Config{Bandwidth: bandwidth, Burst: burst})
	ctx := context.Background()

	start := time.Now()
	err := limiter.RateLimiter().WaitN(ctx, burst)
	if err != nil {
		t.Fatalf("WaitN failed: %v", err)
	}
	burstDuration := time.Since(start)

	if burstDuration > 100*time.Millisecond {
		t.Errorf("Burst should be instant, took %v", burstDuration)
	}

	start = time.Now()
	err = limiter.RateLimiter().WaitN(ctx, 10*1024)
	if err != nil {
		t.Fatalf("WaitN failed: %v", err)
	}
	limitedDuration := time.Since(start)

	if limitedDuration < 900*time.Millisecond || limitedDuration > 1200*time.Millisecond {
		t.Errorf("Rate limiting not working correctly, took %v (expected ~1s)", limitedDuration)
	}

	t.Logf("Burst: %v, Rate-limited: %v", burstDuration, limitedDuration)
}

func TestMultipleBurstMultipliers(t *testing.T) {
	tests := []struct {
		name       string
		bandwidth  int64
		multiplier float64
	}{
		{"1x burst", 10 * 1024, 1.0},
		{"1.5x burst", 10 * 1024, 1.5},
		{"2x burst", 10 * 1024, 2.0},
		{"2.5x burst", 10 * 1024, 2.5},
		{"3x burst", 10 * 1024, 3.0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			burst := int(float64(tt.bandwidth) * tt.multiplier)
			limiter := NewLimiter(Config{Bandwidth: tt.bandwidth, Burst: burst})

			if !limiter.IsLimited() {
				t.Error("Limiter should be limited")
			}

			actualBurst := limiter.RateLimiter().Burst()
			if actualBurst != burst {
				t.Errorf("Burst = %d, want %d", actualBurst, burst)
			}

			ctx := context.Background()
			start := time.Now()
			err := limiter.RateLimiter().WaitN(ctx, burst)
			if err != nil {
				t.Fatalf("WaitN failed: %v", err)
			}
			duration := time.Since(start)

			if duration > 50*time.Millisecond {
				t.Errorf("Burst should be instant, took %v", duration)
			}
		})
	}
}
