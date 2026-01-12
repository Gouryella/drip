package config

import (
	"crypto/tls"
	"fmt"
	"os"
	"strings"
)

// ServerConfig holds the server configuration
type ServerConfig struct {
	// Server settings
	Port       int
	PublicPort int // Port to display in URLs (for reverse proxy scenarios)
	Domain     string

	// TCP tunnel dynamic port allocation
	TCPPortMin int
	TCPPortMax int

	// TLS/SSL settings
	TLSEnabled  bool
	TLSCertFile string
	TLSKeyFile  string
	AutoTLS     bool // Automatic Let's Encrypt

	// Security
	AuthToken string

	// Logging
	Debug bool
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

// LoadTLSConfig loads TLS configuration
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

// GetClientTLSConfig returns TLS config for client connections
func GetClientTLSConfig(serverName string) *tls.Config {
	return &tls.Config{
		ServerName:               serverName,
		MinVersion:               tls.VersionTLS13, // Only TLS 1.3
		MaxVersion:               tls.VersionTLS13, // Only TLS 1.3
		ClientSessionCache:       tls.NewLRUClientSessionCache(0), // Enable session resumption (0 = default size)
		PreferServerCipherSuites: true,                            // Prefer server cipher suites (ignored in TLS 1.3 but set for consistency)
		CipherSuites: []uint16{
			tls.TLS_AES_128_GCM_SHA256,
			tls.TLS_AES_256_GCM_SHA384,
			tls.TLS_CHACHA20_POLY1305_SHA256,
		},
	}
}

// GetClientTLSConfigInsecure returns TLS config for client with InsecureSkipVerify
// WARNING: Only use for testing!
func GetClientTLSConfigInsecure() *tls.Config {
	return &tls.Config{
		InsecureSkipVerify:       true,
		MinVersion:               tls.VersionTLS13, // Only TLS 1.3
		MaxVersion:               tls.VersionTLS13, // Only TLS 1.3
		ClientSessionCache:       tls.NewLRUClientSessionCache(0), // Enable session resumption (0 = default size)
		PreferServerCipherSuites: true,                            // Prefer server cipher suites (ignored in TLS 1.3 but set for consistency)
		CipherSuites: []uint16{
			tls.TLS_AES_128_GCM_SHA256,
			tls.TLS_AES_256_GCM_SHA384,
			tls.TLS_CHACHA20_POLY1305_SHA256,
		},
	}
}

// GetServerURL returns the server URL based on configuration
func (c *ServerConfig) GetServerURL() string {
	protocol := "http"
	if c.TLSEnabled {
		protocol = "https"
	}

	if c.Port == 80 || (c.TLSEnabled && c.Port == 443) {
		return fmt.Sprintf("%s://%s", protocol, c.Domain)
	}

	return fmt.Sprintf("%s://%s:%d", protocol, c.Domain, c.Port)
}

// GetTCPAddress returns the TCP address for tunnel connections
func (c *ServerConfig) GetTCPAddress() string {
	return fmt.Sprintf("%s:%d", c.Domain, c.Port)
}
