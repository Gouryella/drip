package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestServerConfigBandwidth(t *testing.T) {
	tests := []struct {
		name           string
		yaml           string
		wantBandwidth  string
		wantMultiplier float64
		wantBodyLimit  int64
		wantTrusted    []string
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
trusted_proxies:
  - 127.0.0.1
  - 10.0.0.0/8
`,
			wantBandwidth:  "1M",
			wantMultiplier: 2.5,
			wantBodyLimit:  10485760,
			wantTrusted:    []string{"127.0.0.1", "10.0.0.0/8"},
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
			wantBodyLimit:  DefaultMaxRequestBodyBytes,
		},
		{
			name: "explicit unlimited body limit",
			yaml: `
port: 8443
domain: example.com
tcp_port_min: 10000
tcp_port_max: 20000
max_request_body_bytes: 0
`,
			wantBandwidth:  "",
			wantMultiplier: 0,
			wantBodyLimit:  0,
			wantTrusted:    nil,
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
			if len(cfg.TrustedProxies) != len(tt.wantTrusted) {
				t.Fatalf("TrustedProxies len = %d, want %d", len(cfg.TrustedProxies), len(tt.wantTrusted))
			}
			for i := range tt.wantTrusted {
				if cfg.TrustedProxies[i] != tt.wantTrusted[i] {
					t.Errorf("TrustedProxies[%d] = %q, want %q", i, cfg.TrustedProxies[i], tt.wantTrusted[i])
				}
			}
			if tt.wantBodyLimit == 0 && !cfg.MaxRequestBodyBytesExplicit() {
				t.Errorf("MaxRequestBodyBytesExplicit() = false, want true")
			}
		})
	}
}

func TestServerConfigValidateRejectsNegativeRequestBodyLimit(t *testing.T) {
	cfg := validServerConfig()
	cfg.MaxRequestBodyBytes = -1

	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() expected error for negative MaxRequestBodyBytes")
	}
}

func TestLoadServerConfigRejectsUnknownField(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	data := []byte(`
port: 8443
domain: example.com
tcp_port_min: 10000
tcp_port_max: 20000
unknown_field: true
`)
	if err := os.WriteFile(configPath, data, 0600); err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}

	_, err := LoadServerConfig(configPath)
	if err == nil {
		t.Fatal("LoadServerConfig expected error for unknown field")
	}
	if !strings.Contains(err.Error(), "unknown_field") {
		t.Fatalf("LoadServerConfig error = %v, want unknown_field", err)
	}
}

func TestLoadClientConfigRejectsUnknownNestedTunnelField(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	data := []byte(`
server: example.com:443
token: real-client-token
tls: true
tunnels:
  - name: app
    type: http
    port: 3000
    unexpected_tunnel_field: true
`)
	if err := os.WriteFile(configPath, data, 0600); err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}

	_, err := LoadClientConfig(configPath)
	if err == nil {
		t.Fatal("LoadClientConfig expected error for unknown nested tunnel field")
	}
	if !strings.Contains(err.Error(), "unexpected_tunnel_field") {
		t.Fatalf("LoadClientConfig error = %v, want unexpected_tunnel_field", err)
	}
}

func TestServerConfigValidateRejectsPlaceholderSecrets(t *testing.T) {
	tests := []struct {
		name         string
		authToken    string
		metricsToken string
		want         string
	}{
		{
			name:      "auth token placeholder",
			authToken: "your-secret-token",
			want:      "token uses a placeholder value",
		},
		{
			name:         "metrics token placeholder",
			authToken:    "real-server-token",
			metricsToken: "secret",
			want:         "metrics_token uses a placeholder value",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validServerConfig()
			cfg.AuthToken = tt.authToken
			cfg.MetricsToken = tt.metricsToken

			err := cfg.Validate()
			if err == nil {
				t.Fatal("Validate() expected error for placeholder secret")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestTunnelConfigValidateRejectsInvalidIPAccessRule(t *testing.T) {
	tunnel := &TunnelConfig{
		Name:     "app",
		Type:     "http",
		Port:     3000,
		AllowIPs: []string{"999.999.999.999"},
	}

	err := tunnel.Validate()
	if err == nil {
		t.Fatal("Validate() expected error for invalid allow IP")
	}
	if !strings.Contains(err.Error(), "invalid IP access rules") {
		t.Fatalf("Validate() error = %v, want invalid IP access rules", err)
	}
}

func TestLoadServerConfigRejectsBroadPermissionsWhenTokenSet(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits are not reliable on Windows")
	}

	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	data := []byte(`
port: 8443
domain: example.com
tcp_port_min: 10000
tcp_port_max: 20000
token: real-server-token
`)
	if err := os.WriteFile(configPath, data, 0600); err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}
	if err := os.Chmod(configPath, 0644); err != nil {
		t.Fatalf("Failed to chmod config file: %v", err)
	}

	_, err := LoadServerConfig(configPath)
	if err == nil {
		t.Fatal("LoadServerConfig expected error for broad secret file permissions")
	}
	if !strings.Contains(err.Error(), "overly broad permissions") {
		t.Fatalf("LoadServerConfig error = %v, want permissions error", err)
	}
}

func TestLoadServerConfigAllowsCurrentGroupReadWhenTokenSet(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits are not reliable on Windows")
	}

	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	data := []byte(`
port: 8443
domain: example.com
tcp_port_min: 10000
tcp_port_max: 20000
token: real-server-token
`)
	if err := os.WriteFile(configPath, data, 0600); err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}
	if err := os.Chmod(configPath, 0640); err != nil {
		t.Fatalf("Failed to chmod config file: %v", err)
	}

	if _, err := LoadServerConfig(configPath); err != nil {
		t.Fatalf("LoadServerConfig expected current-group readable config to be valid, got: %v", err)
	}
}

func TestSensitiveFilePermissionsAllowReadOnlyDockerSecret(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits are not reliable on Windows")
	}

	tmpDir := t.TempDir()
	secretPath := filepath.Join(tmpDir, "secret")
	if err := os.WriteFile(secretPath, []byte("secret"), 0600); err != nil {
		t.Fatalf("Failed to write secret file: %v", err)
	}
	if err := os.Chmod(secretPath, 0444); err != nil {
		t.Fatalf("Failed to chmod secret file: %v", err)
	}
	info, err := os.Stat(secretPath)
	if err != nil {
		t.Fatalf("Failed to stat secret file: %v", err)
	}

	if err := checkSensitiveFilePermissions("/run/secrets/drip_tls_key", info, "TLS key file"); err != nil {
		t.Fatalf("checkSensitiveFilePermissions() expected read-only Docker secret to be valid, got: %v", err)
	}
}

func TestLoadClientConfigRejectsBroadPermissionsWhenTokenSet(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits are not reliable on Windows")
	}

	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	data := []byte(`
server: example.com:443
token: real-client-token
tls: true
`)
	if err := os.WriteFile(configPath, data, 0600); err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}
	if err := os.Chmod(configPath, 0644); err != nil {
		t.Fatalf("Failed to chmod config file: %v", err)
	}

	_, err := LoadClientConfig(configPath)
	if err == nil {
		t.Fatal("LoadClientConfig expected error for broad secret file permissions")
	}
	if !strings.Contains(err.Error(), "overly broad permissions") {
		t.Fatalf("LoadClientConfig error = %v, want permissions error", err)
	}
}

func TestLoadTLSConfigRejectsBroadKeyPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits are not reliable on Windows")
	}

	tmpDir := t.TempDir()
	certPath := filepath.Join(tmpDir, "cert.pem")
	keyPath := filepath.Join(tmpDir, "key.pem")
	if err := os.WriteFile(certPath, []byte("not a real cert"), 0600); err != nil {
		t.Fatalf("Failed to write cert file: %v", err)
	}
	if err := os.WriteFile(keyPath, []byte("not a real key"), 0600); err != nil {
		t.Fatalf("Failed to write key file: %v", err)
	}
	if err := os.Chmod(keyPath, 0644); err != nil {
		t.Fatalf("Failed to chmod key file: %v", err)
	}

	cfg := &ServerConfig{
		TLSEnabled:  true,
		TLSCertFile: certPath,
		TLSKeyFile:  keyPath,
	}

	_, err := cfg.LoadTLSConfig()
	if err == nil {
		t.Fatal("LoadTLSConfig expected error for broad key file permissions")
	}
	if !strings.Contains(err.Error(), "overly broad permissions") {
		t.Fatalf("LoadTLSConfig error = %v, want permissions error", err)
	}
}

func TestServerConfigValidateRejectsInvalidTrustedProxy(t *testing.T) {
	cfg := validServerConfig()
	cfg.TrustedProxies = []string{"not-an-ip"}

	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() expected error for invalid trusted proxy")
	}
}

func TestServerConfigValidateRequiresAuthTokenByDefault(t *testing.T) {
	cfg := validServerConfig()
	cfg.AuthToken = ""

	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() expected error when token is empty and anonymous access is disabled")
	}
}

func TestServerConfigValidateAllowsExplicitAnonymousAccess(t *testing.T) {
	cfg := validServerConfig()
	cfg.AuthToken = ""
	cfg.AllowAnonymous = true

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() expected explicit anonymous access to be valid, got: %v", err)
	}
}

func TestServerConfigValidateAcceptsAuthToken(t *testing.T) {
	cfg := validServerConfig()

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() expected token-authenticated config to be valid, got: %v", err)
	}
}

func validServerConfig() *ServerConfig {
	return &ServerConfig{
		Port:                8443,
		Domain:              "example.com",
		AuthToken:           "real-server-token",
		TCPPortMin:          10000,
		TCPPortMax:          20000,
		MaxRequestBodyBytes: DefaultMaxRequestBodyBytes,
	}
}
