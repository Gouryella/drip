package config

import (
	"crypto/tls"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type ServerConfig struct {
	Port         int    `yaml:"port"`
	PublicPort   int    `yaml:"public_port"`   // Port to display in URLs (for reverse proxy scenarios)
	Domain       string `yaml:"domain"`        // Domain for client connections (e.g., connect.example.com)
	TunnelDomain string `yaml:"tunnel_domain"` // Domain for tunnel URLs (e.g., example.com for *.example.com)

	// TCP tunnel dynamic port allocation
	TCPPortMin int `yaml:"tcp_port_min"`
	TCPPortMax int `yaml:"tcp_port_max"`

	// TLS settings
	TLSEnabled  bool   `yaml:"tls_enabled"`
	TLSCertFile string `yaml:"tls_cert"`
	TLSKeyFile  string `yaml:"tls_key"`

	// Security
	AuthToken    string `yaml:"token"`
	MetricsToken string `yaml:"metrics_token"`

	// Logging
	Debug bool `yaml:"debug"`

	// Performance
	PprofPort int `yaml:"pprof_port"`

	// Allowed transports: "tcp", "wss", or "tcp,wss" (default: "tcp,wss")
	AllowedTransports  []string `yaml:"transports"`
	AllowedTunnelTypes []string `yaml:"tunnel_types"`
	Bandwidth          string   `yaml:"bandwidth,omitempty"`
	BurstMultiplier    float64  `yaml:"burst_multiplier,omitempty"`
}

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
	if c.TLSEnabled {
		if c.TLSCertFile == "" {
			return fmt.Errorf("TLS certificate file is required when TLS is enabled")
		}
		if c.TLSKeyFile == "" {
			return fmt.Errorf("TLS key file is required when TLS is enabled")
		}
	}

	return nil
}

func (c *ServerConfig) LoadTLSConfig() (*tls.Config, error) {
	if !c.TLSEnabled {
		return nil, nil
	}

	if c.TLSCertFile == "" || c.TLSKeyFile == "" {
		return nil, fmt.Errorf("TLS enabled but certificate files not specified")
	}

	if _, err := os.Stat(c.TLSCertFile); os.IsNotExist(err) {
		return nil, fmt.Errorf("certificate file not found: %s", c.TLSCertFile)
	}

	if _, err := os.Stat(c.TLSKeyFile); os.IsNotExist(err) {
		return nil, fmt.Errorf("key file not found: %s", c.TLSKeyFile)
	}

	cert, err := tls.LoadX509KeyPair(c.TLSCertFile, c.TLSKeyFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load certificate: %w", err)
	}

	// Force TLS 1.3 only
	tlsConfig := &tls.Config{
		Certificates:             []tls.Certificate{cert},
		MinVersion:               tls.VersionTLS13, // Only TLS 1.3
		MaxVersion:               tls.VersionTLS13, // Only TLS 1.3
		PreferServerCipherSuites: true,             // Prefer server cipher suites (ignored in TLS 1.3 but set for consistency)
		CipherSuites: []uint16{
			tls.TLS_AES_128_GCM_SHA256,
			tls.TLS_AES_256_GCM_SHA384,
			tls.TLS_CHACHA20_POLY1305_SHA256,
		},
	}

	return tlsConfig, nil
}

func GetClientTLSConfig(serverName string) *tls.Config {
	return &tls.Config{
		ServerName:               serverName,
		MinVersion:               tls.VersionTLS13,
		MaxVersion:               tls.VersionTLS13,
		ClientSessionCache:       tls.NewLRUClientSessionCache(0),
		PreferServerCipherSuites: true,
		CipherSuites: []uint16{
			tls.TLS_AES_128_GCM_SHA256,
			tls.TLS_AES_256_GCM_SHA384,
			tls.TLS_CHACHA20_POLY1305_SHA256,
		},
	}
}

func GetClientTLSConfigInsecure() *tls.Config {
	return &tls.Config{
		InsecureSkipVerify:       true,
		MinVersion:               tls.VersionTLS13,
		MaxVersion:               tls.VersionTLS13,
		ClientSessionCache:       tls.NewLRUClientSessionCache(0),
		PreferServerCipherSuites: true,
		CipherSuites: []uint16{
			tls.TLS_AES_128_GCM_SHA256,
			tls.TLS_AES_256_GCM_SHA384,
			tls.TLS_CHACHA20_POLY1305_SHA256,
		},
	}
}

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

func LoadServerConfig(path string) (*ServerConfig, error) {
	if path == "" {
		path = DefaultServerConfigPath()
	}

	data, err := os.ReadFile(path)
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

func SaveServerConfig(config *ServerConfig, path string) error {
	if path == "" {
		path = DefaultServerConfigPath()
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	data, err := yaml.Marshal(config)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}

	return nil
}

func ServerConfigExists(path string) bool {
	if path == "" {
		path = DefaultServerConfigPath()
	}
	_, err := os.Stat(path)
	return err == nil
}
