package tcp

import (
	"testing"
)

func TestEffectiveBandwidthSelection(t *testing.T) {
	tests := []struct {
		name           string
		serverBW       int64
		clientBW       int64
		wantEffective  int64
	}{
		{
			name:          "server only",
			serverBW:      1024 * 1024,
			clientBW:      0,
			wantEffective: 1024 * 1024,
		},
		{
			name:          "client only",
			serverBW:      0,
			clientBW:      512 * 1024,
			wantEffective: 512 * 1024,
		},
		{
			name:          "both unlimited",
			serverBW:      0,
			clientBW:      0,
			wantEffective: 0,
		},
		{
			name:          "client lower than server",
			serverBW:      10 * 1024 * 1024,
			clientBW:      1 * 1024 * 1024,
			wantEffective: 1 * 1024 * 1024,
		},
		{
			name:          "client higher than server - server wins",
			serverBW:      1 * 1024 * 1024,
			clientBW:      10 * 1024 * 1024,
			wantEffective: 1 * 1024 * 1024,
		},
		{
			name:          "client equal to server",
			serverBW:      5 * 1024 * 1024,
			clientBW:      5 * 1024 * 1024,
			wantEffective: 5 * 1024 * 1024,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			effectiveBandwidth := tt.serverBW
			if tt.clientBW > 0 {
				if effectiveBandwidth == 0 || tt.clientBW < effectiveBandwidth {
					effectiveBandwidth = tt.clientBW
				}
			}

			if effectiveBandwidth != tt.wantEffective {
				t.Errorf("effectiveBandwidth = %d, want %d", effectiveBandwidth, tt.wantEffective)
			}
		})
	}
}

func TestConnectionSetBandwidthConfig(t *testing.T) {
	tests := []struct {
		name            string
		bandwidth       int64
		burstMultiplier float64
		wantBandwidth   int64
		wantMultiplier  float64
	}{
		{
			name:            "1MB/s with 2x burst",
			bandwidth:       1024 * 1024,
			burstMultiplier: 2.0,
			wantBandwidth:   1024 * 1024,
			wantMultiplier:  2.0,
		},
		{
			name:            "1MB/s with 2.5x burst",
			bandwidth:       1024 * 1024,
			burstMultiplier: 2.5,
			wantBandwidth:   1024 * 1024,
			wantMultiplier:  2.5,
		},
		{
			name:            "default multiplier when 0",
			bandwidth:       1024 * 1024,
			burstMultiplier: 0,
			wantBandwidth:   1024 * 1024,
			wantMultiplier:  2.0,
		},
		{
			name:            "default multiplier when negative",
			bandwidth:       1024 * 1024,
			burstMultiplier: -1.0,
			wantBandwidth:   1024 * 1024,
			wantMultiplier:  2.0,
		},
		{
			name:            "unlimited bandwidth",
			bandwidth:       0,
			burstMultiplier: 2.5,
			wantBandwidth:   0,
			wantMultiplier:  2.5,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn := &Connection{}
			conn.SetBandwidthConfig(tt.bandwidth, tt.burstMultiplier)

			if conn.bandwidth != tt.wantBandwidth {
				t.Errorf("bandwidth = %v, want %v", conn.bandwidth, tt.wantBandwidth)
			}

			if conn.burstMultiplier != tt.wantMultiplier {
				t.Errorf("burstMultiplier = %v, want %v", conn.burstMultiplier, tt.wantMultiplier)
			}
		})
	}
}

func TestListenerBandwidthConfig(t *testing.T) {
	tests := []struct {
		name            string
		bandwidth       int64
		burstMultiplier float64
		wantBandwidth   int64
		wantMultiplier  float64
	}{
		{
			name:            "set bandwidth and multiplier",
			bandwidth:       1024 * 1024,
			burstMultiplier: 2.5,
			wantBandwidth:   1024 * 1024,
			wantMultiplier:  2.5,
		},
		{
			name:            "default multiplier",
			bandwidth:       1024 * 1024,
			burstMultiplier: 0,
			wantBandwidth:   1024 * 1024,
			wantMultiplier:  2.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := &Listener{}
			l.SetBandwidth(tt.bandwidth)
			l.SetBurstMultiplier(tt.burstMultiplier)

			if l.bandwidth != tt.wantBandwidth {
				t.Errorf("bandwidth = %v, want %v", l.bandwidth, tt.wantBandwidth)
			}

			if l.burstMultiplier != tt.wantMultiplier {
				t.Errorf("burstMultiplier = %v, want %v", l.burstMultiplier, tt.wantMultiplier)
			}
		})
	}
}
