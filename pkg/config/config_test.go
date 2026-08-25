package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestServerConfigBandwidth(t *testing.T) {
	tests := []struct {
		name           string
		yaml           string
		wantBandwidth  string
		wantMultiplier float64
		wantBodyLimit  int64
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
max_request_body_bytes: 10485760
`,
			wantBandwidth:  "1M",
			wantMultiplier: 2.5,
			wantBodyLimit:  10485760,
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
			wantBodyLimit:  0,
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
			if cfg.MaxRequestBodyBytes != tt.wantBodyLimit {
				t.Errorf("MaxRequestBodyBytes = %d, want %d", cfg.MaxRequestBodyBytes, tt.wantBodyLimit)
			}
		})
	}
}

func TestServerConfigValidateRejectsNegativeRequestBodyLimit(t *testing.T) {
	cfg := &ServerConfig{
		Port:                8443,
		Domain:              "example.com",
		TCPPortMin:          10000,
		TCPPortMax:          20000,
		MaxRequestBodyBytes: -1,
	}

	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() expected error for negative MaxRequestBodyBytes")
	}
}

func baseValidServerConfig() *ServerConfig {
	return &ServerConfig{
		Port:       8443,
		Domain:     "example.com",
		TCPPortMin: 30000,
		TCPPortMax: 30100,
	}
}

func TestResolveTLSMode(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*ServerConfig)
		want    string
		wantErr bool
	}{
		{
			name:   "bare config is plain",
			mutate: func(*ServerConfig) {},
			want:   "none",
		},
		{
			name: "certificate pair implies manual",
			mutate: func(c *ServerConfig) {
				c.TLSCertFile = "cert.pem"
				c.TLSKeyFile = "key.pem"
			},
			want: "manual",
		},
		{
			name:   "tls_enabled implies manual",
			mutate: func(c *ServerConfig) { c.TLSEnabled = true },
			want:   "manual",
		},
		{
			name:   "acme credentials imply acme",
			mutate: func(c *ServerConfig) { c.ACME.DNSProvider = "cloudflare" },
			want:   "acme",
		},
		{
			name: "explicit mode wins over inference",
			mutate: func(c *ServerConfig) {
				c.TLSMode = "none"
				c.ACME.DNSProvider = "cloudflare"
				c.TLSEnabled = true
			},
			want: "none",
		},
		{
			name:    "unknown mode is rejected",
			mutate:  func(c *ServerConfig) { c.TLSMode = "autocert" },
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseValidServerConfig()
			tc.mutate(cfg)

			got, err := cfg.ResolveTLSMode()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ResolveTLSMode() = %q, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveTLSMode() error = %v", err)
			}
			if got != tc.want {
				t.Fatalf("ResolveTLSMode() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestValidateTLSModes(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*ServerConfig)
		wantErr bool
	}{
		{
			name:   "plain needs nothing",
			mutate: func(*ServerConfig) {},
		},
		{
			name: "manual without a key is rejected",
			mutate: func(c *ServerConfig) {
				c.TLSMode = "manual"
				c.TLSCertFile = "cert.pem"
			},
			wantErr: true,
		},
		{
			name: "manual with both files is accepted",
			mutate: func(c *ServerConfig) {
				c.TLSCertFile = "cert.pem"
				c.TLSKeyFile = "key.pem"
			},
		},
		{
			name:    "acme without a DNS provider is rejected",
			mutate:  func(c *ServerConfig) { c.TLSMode = "acme" },
			wantErr: true,
		},
		{
			name: "acme without a token is rejected",
			mutate: func(c *ServerConfig) {
				c.ACME.DNSProvider = "cloudflare"
			},
			wantErr: true,
		},
		{
			name: "acme with provider and token is accepted",
			mutate: func(c *ServerConfig) {
				c.ACME.DNSProvider = "cloudflare"
				c.ACME.DNSAPIToken = "token"
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseValidServerConfig()
			tc.mutate(cfg)

			err := cfg.Validate()
			if tc.wantErr && err == nil {
				t.Fatal("Validate() = nil, want error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
		})
	}
}

func TestRequireAuthNeedsACredentialSource(t *testing.T) {
	cfg := baseValidServerConfig()
	cfg.RequireAuth = true

	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() = nil, want an error for require_auth with no credential source")
	}

	cfg.DBPath = "/var/lib/drip/control.db"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}
