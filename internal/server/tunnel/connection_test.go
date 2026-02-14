package tunnel

import (
	"testing"

	"go.uber.org/zap"
)

func TestConnectionBandwidthWithBurst(t *testing.T) {
	logger := zap.NewNop()

	tests := []struct {
		name            string
		bandwidth       int64
		burstMultiplier float64
		wantBandwidth   int64
		wantBurst       int
	}{
		{
			name:            "1MB/s with 2x burst",
			bandwidth:       1024 * 1024,
			burstMultiplier: 2.0,
			wantBandwidth:   1024 * 1024,
			wantBurst:       2 * 1024 * 1024,
		},
		{
			name:            "1MB/s with 2.5x burst",
			bandwidth:       1024 * 1024,
			burstMultiplier: 2.5,
			wantBandwidth:   1024 * 1024,
			wantBurst:       int(float64(1024*1024) * 2.5),
		},
		{
			name:            "500KB/s with 3x burst",
			bandwidth:       500 * 1024,
			burstMultiplier: 3.0,
			wantBandwidth:   500 * 1024,
			wantBurst:       3 * 500 * 1024,
		},
		{
			name:            "10MB/s with 1.5x burst",
			bandwidth:       10 * 1024 * 1024,
			burstMultiplier: 1.5,
			wantBandwidth:   10 * 1024 * 1024,
			wantBurst:       int(float64(10*1024*1024) * 1.5),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn := NewConnection("test-subdomain", nil, logger)

			conn.SetBandwidthWithBurst(tt.bandwidth, tt.burstMultiplier)

			if conn.GetBandwidth() != tt.wantBandwidth {
				t.Errorf("GetBandwidth() = %v, want %v", conn.GetBandwidth(), tt.wantBandwidth)
			}

			limiter := conn.GetLimiter()
			if limiter == nil {
				t.Fatal("GetLimiter() should not be nil")
			}

			if !limiter.IsLimited() {
				t.Error("Limiter should be limited")
			}

			if limiter.RateLimiter().Burst() != tt.wantBurst {
				t.Errorf("Burst() = %v, want %v", limiter.RateLimiter().Burst(), tt.wantBurst)
			}
		})
	}
}

func TestConnectionBandwidthUnlimited(t *testing.T) {
	logger := zap.NewNop()
	conn := NewConnection("test-subdomain", nil, logger)

	if conn.GetBandwidth() != 0 {
		t.Errorf("Default bandwidth should be 0, got %v", conn.GetBandwidth())
	}

	if conn.GetLimiter() != nil {
		t.Error("Default limiter should be nil")
	}

	conn.SetBandwidth(0)
	if conn.GetLimiter() != nil {
		t.Error("Limiter should be nil when bandwidth is 0")
	}

	conn.SetBandwidthWithBurst(0, 2.0)
	if conn.GetLimiter() != nil {
		t.Error("Limiter should be nil when bandwidth is 0")
	}
}

func TestConnectionSetBandwidth(t *testing.T) {
	logger := zap.NewNop()
	conn := NewConnection("test-subdomain", nil, logger)

	conn.SetBandwidth(1024 * 1024)

	if conn.GetBandwidth() != 1024*1024 {
		t.Errorf("GetBandwidth() = %v, want %v", conn.GetBandwidth(), 1024*1024)
	}

	limiter := conn.GetLimiter()
	if limiter == nil {
		t.Fatal("GetLimiter() should not be nil")
	}

	expectedBurst := 2 * 1024 * 1024
	if limiter.RateLimiter().Burst() != expectedBurst {
		t.Errorf("Burst() = %v, want %v", limiter.RateLimiter().Burst(), expectedBurst)
	}
}
