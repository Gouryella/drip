package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestServerConfigBandwidth(t *testing.T) {
	tests := []struct {
		name           string
		yaml           string
		wantBandwidth  string
		wantMultiplier float64
	}{
		{
			name: "bandwidth 1M with 2.5x burst",
			yaml: `
port: 8443
domain: example.com
tcp_port_min: 10000
tcp_port_max: 20000
bandwidth: 1M
burst_multiplier: 2.5
`,
			wantBandwidth:  "1M",
			wantMultiplier: 2.5,
		},
		{
			name: "bandwidth 10M with default burst",
			yaml: `
port: 8443
domain: example.com
tcp_port_min: 10000
tcp_port_max: 20000
bandwidth: 10M
`,
			wantBandwidth:  "10M",
			wantMultiplier: 0, // not set, will use default 2.0 in code
		},
		{
			name: "no bandwidth limit",
			yaml: `
port: 8443
domain: example.com
tcp_port_min: 10000
tcp_port_max: 20000
`,
			wantBandwidth:  "",
			wantMultiplier: 0,
		},
		{
			name: "bandwidth 500K with 3x burst",
			yaml: `
port: 8443
domain: example.com
tcp_port_min: 10000
tcp_port_max: 20000
bandwidth: 500K
burst_multiplier: 3.0
`,
			wantBandwidth:  "500K",
			wantMultiplier: 3.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			configPath := filepath.Join(tmpDir, "config.yaml")
			if err := os.WriteFile(configPath, []byte(tt.yaml), 0600); err != nil {
				t.Fatalf("Failed to write config file: %v", err)
			}

			cfg, err := LoadServerConfig(configPath)
			if err != nil {
				t.Fatalf("LoadServerConfig failed: %v", err)
			}

			if cfg.Bandwidth != tt.wantBandwidth {
				t.Errorf("Bandwidth = %q, want %q", cfg.Bandwidth, tt.wantBandwidth)
			}

			if cfg.BurstMultiplier != tt.wantMultiplier {
				t.Errorf("BurstMultiplier = %v, want %v", cfg.BurstMultiplier, tt.wantMultiplier)
			}
		})
	}
}

func TestParseBandwidth(t *testing.T) {
	tests := []struct {
		input string
		want  int64
	}{
		{"", 0},
		{"0", 0},
		{"1024", 1024},
		{"1K", 1024},
		{"1KB", 1024},
		{"1k", 1024},
		{"1M", 1024 * 1024},
		{"1MB", 1024 * 1024},
		{"1m", 1024 * 1024},
		{"10M", 10 * 1024 * 1024},
		{"1G", 1024 * 1024 * 1024},
		{"1GB", 1024 * 1024 * 1024},
		{"500K", 500 * 1024},
		{"100M", 100 * 1024 * 1024},
		{" 1M ", 1024 * 1024}, // with spaces
		{"invalid", 0},
		{"-1M", 0}, // negative
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := parseBandwidthString(tt.input)
			if got != tt.want {
				t.Errorf("parseBandwidthString(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func parseBandwidthString(s string) int64 {
	if s == "" {
		return 0
	}

	s = strings.TrimSpace(strings.ToUpper(s))
	if s == "" {
		return 0
	}

	var multiplier int64 = 1
	switch {
	case strings.HasSuffix(s, "GB") || strings.HasSuffix(s, "G"):
		multiplier = 1024 * 1024 * 1024
		s = strings.TrimSuffix(strings.TrimSuffix(s, "GB"), "G")
	case strings.HasSuffix(s, "MB") || strings.HasSuffix(s, "M"):
		multiplier = 1024 * 1024
		s = strings.TrimSuffix(strings.TrimSuffix(s, "MB"), "M")
	case strings.HasSuffix(s, "KB") || strings.HasSuffix(s, "K"):
		multiplier = 1024
		s = strings.TrimSuffix(strings.TrimSuffix(s, "KB"), "K")
	case strings.HasSuffix(s, "B"):
		s = strings.TrimSuffix(s, "B")
	}

	val, err := strconv.ParseInt(s, 10, 64)
	if err != nil || val < 0 {
		return 0
	}

	return val * multiplier
}
