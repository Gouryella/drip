package config

import (
	"crypto/tls"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// ServerConfig holds the server configuration
type ServerConfig struct {
	Port         int    `yaml:"port"`
	PublicPort   int    `yaml:"public_port"`   // Port to display in URLs (for reverse proxy scenarios)
	Domain       string `yaml:"domain"`        // Domain for client connections (e.g., connect.example.com)
	TunnelDomain string `yaml:"tunnel_domain"` // Domain for tunnel URLs (e.g., example.com for *.example.com)

	// TCP tunnel dynamic port allocation
	TCPPortMin int `yaml:"tcp_port_min"`
	TCPPortMax int `yaml:"tcp_port_max"`

	// TLS settings
	// TLSMode selects how the server obtains its certificate: "none" (a reverse
	// proxy terminates TLS), "manual" (tls_cert/tls_key), or "acme" (the server
	// obtains a DNS-01 wildcard itself). Empty is inferred - see ResolveTLSMode.
	TLSMode     string `yaml:"tls_mode,omitempty"`
	TLSEnabled  bool   `yaml:"tls_enabled"`
	TLSCertFile string `yaml:"tls_cert"`
	TLSKeyFile  string `yaml:"tls_key"`

	// ACME configures certificate issuance when TLSMode is "acme".
	ACME ACMEConfig `yaml:"acme,omitempty"`

	// Security
	AuthToken    string `yaml:"token"`
	MetricsToken string `yaml:"metrics_token"`

	// Control plane
	// DBPath enables the SQLite control plane (accounts, client credentials,
	// tunnel reservations, admin users). Empty keeps the server stateless and
	// falls back to the shared token in AuthToken.
	DBPath string `yaml:"db_path,omitempty"`
	// ReservationsOnly rejects any registration that does not bind a
	// reservation, turning the deployment into a closed fleet where every
	// tunnel is pre-allocated in the admin panel.
	ReservationsOnly bool `yaml:"reservations_only,omitempty"`
	// AdminAddress enables the admin panel on its own listener, e.g.
	// "127.0.0.1:8444". Administrative traffic never shares a port with tunnel
	// traffic. The loopback default is deliberate: the panel has a first-run
	// setup screen that is unauthenticated by necessity, so it should be
	// reached over a VPN or an SSH tunnel rather than published.
	AdminAddress string `yaml:"admin_address,omitempty"`
	// AdminSessionHours bounds a signed-in panel session. 0 uses 12 hours.
	AdminSessionHours int `yaml:"admin_session_hours,omitempty"`
	// RequireAuth rejects registrations that carry no recognised credential.
	// Without it a server with neither DBPath nor AuthToken accepts anyone,
	// which is the historical single-user self-hosted default.
	RequireAuth bool `yaml:"require_auth,omitempty"`

	// Logging
	Debug bool `yaml:"debug"`

	// Performance
	PprofPort int `yaml:"pprof_port"`

	// Allowed transports: "tcp", "wss", or "tcp,wss" (default: "tcp,wss")
	AllowedTransports []string `yaml:"transports"`

	// Allowed tunnel types: "http", "https", "tcp" (default: all)
	AllowedTunnelTypes []string `yaml:"tunnel_types"`

	// Bandwidth limiting
	Bandwidth       string  `yaml:"bandwidth,omitempty"`
	BurstMultiplier float64 `yaml:"burst_multiplier,omitempty"`

	// Optional HTTP request body limit for tunneled HTTP/HTTPS traffic.
	// 0 disables the limit and preserves full reverse-proxy behavior.
	MaxRequestBodyBytes int64 `yaml:"max_request_body_bytes,omitempty"`
}

// ACMEConfig configures automatic certificate issuance.
//
// A wildcard certificate requires the DNS-01 challenge, which requires API
// credentials for the zone hosting the tunnel domain. HTTP-01 cannot issue
// wildcards, so there is no credential-free path to covering every tunnel
// subdomain with one certificate.
type ACMEConfig struct {
	// Email is the ACME account contact used for expiry notices.
	Email string `yaml:"email,omitempty"`
	// Domains overrides the certificate names, which otherwise default to the
	// server domain, the tunnel domain, and *.<tunnel domain>.
	Domains []string `yaml:"domains,omitempty"`
	// DNSProvider names the DNS API to use, e.g. "cloudflare".
	DNSProvider string `yaml:"dns_provider,omitempty"`
	// DNSAPIToken is the provider's scoped API token.
	DNSAPIToken string `yaml:"dns_api_token,omitempty"`
	// CA is "production" (default), "staging", or an ACME directory URL. Use
	// staging while testing: production issuance is rate limited to 50
	// certificates per registered domain per week.
	CA string `yaml:"ca,omitempty"`
	// CacheDir stores certificates and the ACME account key.
	CacheDir string `yaml:"cache_dir,omitempty"`
	// PropagationTimeoutSeconds bounds the wait for the challenge TXT record to
	// become visible. 0 uses the two-minute default.
	PropagationTimeoutSeconds int `yaml:"propagation_timeout_seconds,omitempty"`
	// Resolvers optionally pins the DNS servers used for propagation checks,
	// for hosts whose resolver serves stale or split-horizon answers.
	Resolvers []string `yaml:"resolvers,omitempty"`
}

// ResolveTLSMode determines the effective TLS mode.
//
// An explicit tls_mode always wins. Otherwise the mode is inferred so that
// existing configuration files keep working unchanged: ACME credentials imply
// acme, a certificate pair (or tls_enabled) implies manual, and a bare config
// implies none.
func (c *ServerConfig) ResolveTLSMode() (string, error) {
	if c.TLSMode != "" {
		mode := strings.ToLower(strings.TrimSpace(c.TLSMode))
		switch mode {
		case "none", "manual", "acme":
			return mode, nil
		default:
			return "", fmt.Errorf("unknown tls_mode %q: want none, manual or acme", c.TLSMode)
		}
	}

	if c.ACME.DNSProvider != "" {
		return "acme", nil
	}
	if c.TLSEnabled || (c.TLSCertFile != "" && c.TLSKeyFile != "") {
		return "manual", nil
	}
	return "none", nil
}

// Validate checks if the server configuration is valid
func (c *ServerConfig) Validate() error {
	// Validate port
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("invalid port %d: must be between 1 and 65535", c.Port)
	}

	// Validate public port if set
	if c.PublicPort != 0 && (c.PublicPort < 1 || c.PublicPort > 65535) {
		return fmt.Errorf("invalid public port %d: must be between 1 and 65535", c.PublicPort)
	}

	// Validate domain
	if c.Domain == "" {
		return fmt.Errorf("domain is required")
	}
	if strings.Contains(c.Domain, ":") {
		return fmt.Errorf("domain should not contain port, got: %s", c.Domain)
	}

	// Validate tunnel domain if set
	if c.TunnelDomain != "" && strings.Contains(c.TunnelDomain, ":") {
		return fmt.Errorf("tunnel domain should not contain port, got: %s", c.TunnelDomain)
	}

	// Validate TCP port range
	if c.TCPPortMin < 1 || c.TCPPortMin > 65535 {
		return fmt.Errorf("invalid TCPPortMin %d: must be between 1 and 65535", c.TCPPortMin)
	}
	if c.TCPPortMax < 1 || c.TCPPortMax > 65535 {
		return fmt.Errorf("invalid TCPPortMax %d: must be between 1 and 65535", c.TCPPortMax)
	}
	if c.TCPPortMin >= c.TCPPortMax {
		return fmt.Errorf("TCPPortMin (%d) must be less than TCPPortMax (%d)", c.TCPPortMin, c.TCPPortMax)
	}

	// Validate TLS settings
	mode, err := c.ResolveTLSMode()
	if err != nil {
		return err
	}
	switch mode {
	case "manual":
		if c.TLSCertFile == "" {
			return fmt.Errorf("tls_cert is required in manual TLS mode")
		}
		if c.TLSKeyFile == "" {
			return fmt.Errorf("tls_key is required in manual TLS mode")
		}
	case "acme":
		if c.ACME.DNSProvider == "" {
			return fmt.Errorf("acme.dns_provider is required in acme TLS mode: a wildcard certificate needs the DNS-01 challenge")
		}
		if c.ACME.DNSAPIToken == "" {
			return fmt.Errorf("acme.dns_api_token is required in acme TLS mode")
		}
		if c.ACME.PropagationTimeoutSeconds < 0 {
			return fmt.Errorf("acme.propagation_timeout_seconds must be >= 0")
		}
	}

	if c.MaxRequestBodyBytes < 0 {
		return fmt.Errorf("max_request_body_bytes must be >= 0")
	}

	if c.RequireAuth && c.DBPath == "" && c.AuthToken == "" {
		return fmt.Errorf("require_auth needs either db_path or token to be set")
	}

	if c.AdminAddress != "" && c.DBPath == "" {
		return fmt.Errorf("admin_address needs db_path to be set: the panel manages the control plane database")
	}

	if c.AdminSessionHours < 0 {
		return fmt.Errorf("admin_session_hours must be >= 0")
	}

	if c.ReservationsOnly && c.DBPath == "" {
		return fmt.Errorf("reservations_only needs db_path to be set: reservations live in the control plane database")
	}

	return nil
}

// GetClientTLSConfig returns TLS config for client connections
func GetClientTLSConfig(serverName string) *tls.Config {
	return &tls.Config{
		ServerName:         serverName,
		MinVersion:         tls.VersionTLS13,
		MaxVersion:         tls.VersionTLS13,
		ClientSessionCache: tls.NewLRUClientSessionCache(0),
		CipherSuites: []uint16{
			tls.TLS_AES_128_GCM_SHA256,
			tls.TLS_AES_256_GCM_SHA384,
			tls.TLS_CHACHA20_POLY1305_SHA256,
		},
	}
}

var insecureTLSWarnOnce sync.Once

// GetClientTLSConfigInsecure returns TLS config for client with InsecureSkipVerify
// WARNING: Only use for testing! This disables TLS certificate verification.
func GetClientTLSConfigInsecure() *tls.Config {
	insecureTLSWarnOnce.Do(func() {
		log.Println("[SECURITY WARNING] TLS certificate verification is disabled (InsecureSkipVerify=true). " +
			"This makes connections vulnerable to man-in-the-middle attacks. " +
			"Only use this for testing or with trusted endpoints.")
	})
	return &tls.Config{
		InsecureSkipVerify: true, // #nosec G402 -- explicit --insecure/test-only mode; behavior intentionally preserved.
		MinVersion:         tls.VersionTLS13,
		MaxVersion:         tls.VersionTLS13,
		ClientSessionCache: tls.NewLRUClientSessionCache(0),
		CipherSuites: []uint16{
			tls.TLS_AES_128_GCM_SHA256,
			tls.TLS_AES_256_GCM_SHA384,
			tls.TLS_CHACHA20_POLY1305_SHA256,
		},
	}
}

// DefaultServerConfigPath returns the default server configuration path
func DefaultServerConfigPath() string {
	// Check /etc/drip/config.yaml first (system-wide)
	systemPath := "/etc/drip/config.yaml"
	if _, err := os.Stat(systemPath); err == nil {
		return systemPath
	}

	// Fall back to user home directory
	home, err := os.UserHomeDir()
	if err != nil {
		return ".drip/server.yaml"
	}
	return filepath.Join(home, ".drip", "server.yaml")
}

// LoadServerConfig loads server configuration from file
func LoadServerConfig(path string) (*ServerConfig, error) {
	if path == "" {
		path = DefaultServerConfigPath()
	}

	cleanPath := filepath.Clean(path)

	data, err := os.ReadFile(cleanPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("config file not found at %s", path)
		}
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var config ServerConfig
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	return &config, nil
}

// SaveServerConfig saves server configuration to file
// #nosec G117 -- Tokens are intentionally saved to config files with 0600 permissions
func SaveServerConfig(config *ServerConfig, path string) error {
	if path == "" {
		path = DefaultServerConfigPath()
	}

	cleanPath := filepath.Clean(path)

	dir := filepath.Dir(cleanPath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	data, err := yaml.Marshal(config)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	if err := os.WriteFile(cleanPath, data, 0600); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}

	return nil
}

// ServerConfigExists checks if server config file exists
func ServerConfigExists(path string) bool {
	if path == "" {
		path = DefaultServerConfigPath()
	}
	_, err := os.Stat(path)
	return err == nil
}
