package config

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"drip/internal/shared/netutil"
	"gopkg.in/yaml.v3"
)

const (
	// DefaultMaxRequestBodyBytes is the public-server default for tunneled
	// HTTP request bodies. Set max_request_body_bytes to 0 only as an explicit
	// high-risk opt-in for unlimited uploads.
	DefaultMaxRequestBodyBytes int64 = 32 << 20
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
	TLSEnabled  bool   `yaml:"tls_enabled"`
	TLSCertFile string `yaml:"tls_cert"`
	TLSKeyFile  string `yaml:"tls_key"`

	// Security
	AuthToken      string   `yaml:"token"`
	AllowAnonymous bool     `yaml:"allow_anonymous"`
	MetricsToken   string   `yaml:"metrics_token"`
	TrustedProxies []string `yaml:"trusted_proxies"`

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
	// 0 disables the limit and preserves full reverse-proxy behavior. Omitting
	// this field uses DefaultMaxRequestBodyBytes.
	MaxRequestBodyBytes int64 `yaml:"max_request_body_bytes,omitempty"`

	maxRequestBodyBytesSet bool
}

// MaxRequestBodyBytesExplicit reports whether max_request_body_bytes was set
// in the loaded YAML. It is used to preserve explicit 0 = unlimited semantics
// while still applying a safe default when the field is omitted.
func (c *ServerConfig) MaxRequestBodyBytesExplicit() bool {
	return c.maxRequestBodyBytesSet
}

// UnmarshalYAML applies safe defaults before decoding optional fields.
func (c *ServerConfig) UnmarshalYAML(value *yaml.Node) error {
	type serverConfigYAML ServerConfig

	if err := validateYAMLMappingKeys(value, serverConfigYAMLKeys); err != nil {
		return err
	}

	raw := serverConfigYAML{
		MaxRequestBodyBytes: DefaultMaxRequestBodyBytes,
	}
	if err := value.Decode(&raw); err != nil {
		return err
	}

	*c = ServerConfig(raw)
	c.maxRequestBodyBytesSet = yamlMappingHasKey(value, "max_request_body_bytes")
	return nil
}

var serverConfigYAMLKeys = map[string]struct{}{
	"port":                   {},
	"public_port":            {},
	"domain":                 {},
	"tunnel_domain":          {},
	"tcp_port_min":           {},
	"tcp_port_max":           {},
	"tls_enabled":            {},
	"tls_cert":               {},
	"tls_key":                {},
	"token":                  {},
	"allow_anonymous":        {},
	"metrics_token":          {},
	"trusted_proxies":        {},
	"debug":                  {},
	"pprof_port":             {},
	"transports":             {},
	"tunnel_types":           {},
	"bandwidth":              {},
	"burst_multiplier":       {},
	"max_request_body_bytes": {},
}

func validateYAMLMappingKeys(value *yaml.Node, allowed map[string]struct{}) error {
	if value == nil || value.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(value.Content); i += 2 {
		key := value.Content[i].Value
		if _, ok := allowed[key]; !ok {
			return fmt.Errorf("unknown field %q", key)
		}
	}
	return nil
}

func yamlMappingHasKey(value *yaml.Node, key string) bool {
	if value == nil || value.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i+1 < len(value.Content); i += 2 {
		if value.Content[i].Value == key {
			return true
		}
	}
	return false
}

var placeholderSecrets = map[string]bool{
	"changeme":          true,
	"change-me":         true,
	"default":           true,
	"password":          true,
	"secret":            true,
	"your-secret":       true,
	"your-secret-token": true,
	"your-token":        true,
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
	if c.TLSEnabled {
		if c.TLSCertFile == "" {
			return fmt.Errorf("TLS certificate file is required when TLS is enabled")
		}
		if c.TLSKeyFile == "" {
			return fmt.Errorf("TLS key file is required when TLS is enabled")
		}
	}

	if isPlaceholderSecret(c.AuthToken) {
		return fmt.Errorf("token uses a placeholder value; set a real secret or set allow_anonymous to true")
	}
	if isPlaceholderSecret(c.MetricsToken) {
		return fmt.Errorf("metrics_token uses a placeholder value; set a real secret or leave it empty")
	}
	if !c.AllowAnonymous && strings.TrimSpace(c.AuthToken) == "" {
		return fmt.Errorf("token is required unless allow_anonymous is true")
	}

	if c.MaxRequestBodyBytes < 0 {
		return fmt.Errorf("max_request_body_bytes must be >= 0")
	}

	if _, err := netutil.NewTrustedProxySet(c.TrustedProxies); err != nil {
		return err
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

	keyInfo, err := os.Stat(c.TLSKeyFile)
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("key file not found: %s", c.TLSKeyFile)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to stat key file: %w", err)
	}
	if err := checkSensitiveFilePermissions(c.TLSKeyFile, keyInfo, "TLS key file"); err != nil {
		return nil, err
	}

	cert, err := tls.LoadX509KeyPair(c.TLSCertFile, c.TLSKeyFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load certificate: %w", err)
	}

	// Force TLS 1.3 only
	tlsConfig := &tls.Config{
		Certificates:       []tls.Certificate{cert},
		MinVersion:         tls.VersionTLS13,                  // Only TLS 1.3
		MaxVersion:         tls.VersionTLS13,                  // Only TLS 1.3
		ClientSessionCache: tls.NewLRUClientSessionCache(128), // Enable session resumption
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

	info, err := os.Stat(cleanPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("config file not found at %s", path)
		}
		return nil, fmt.Errorf("failed to stat config file: %w", err)
	}

	data, err := os.ReadFile(cleanPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("config file not found at %s", path)
		}
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var config ServerConfig
	if err := decodeYAMLStrict(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	if serverConfigContainsSecrets(&config) {
		if err := checkSensitiveFilePermissions(cleanPath, info, "config file containing token fields"); err != nil {
			return nil, err
		}
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

	if err := writeConfigFile(cleanPath, data); err != nil {
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

func decodeYAMLStrict(data []byte, out interface{}) error {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	err := decoder.Decode(out)
	if err == io.EOF {
		return nil
	}
	if err != nil {
		return err
	}
	var extra interface{}
	if err := decoder.Decode(&extra); err != io.EOF {
		if err != nil {
			return err
		}
		return fmt.Errorf("configuration must contain exactly one YAML document")
	}
	return nil
}

func checkSensitiveFilePermissions(path string, info os.FileInfo, description string) error {
	if runtime.GOOS == "windows" {
		return nil
	}

	perm := info.Mode().Perm()
	if isDockerSecretPath(path) && perm&0333 == 0 {
		return nil
	}

	if perm&^0640 != 0 {
		return fmt.Errorf("%s %s has overly broad permissions %04o; use 0600 or 0640 with a trusted service group", description, path, perm)
	}

	if perm&0040 != 0 {
		gid, ok := fileGID(info)
		if !ok || !processInGroup(gid) {
			return fmt.Errorf("%s %s is group-readable by gid %d, which is not one of this process's groups; use 0600 or chgrp to the service group", description, path, gid)
		}
	}

	return nil
}

func isDockerSecretPath(path string) bool {
	clean := filepath.Clean(path)
	return clean == "/run/secrets" || strings.HasPrefix(clean, "/run/secrets/")
}

func serverConfigContainsSecrets(config *ServerConfig) bool {
	return config.AuthToken != "" || config.MetricsToken != ""
}

func isPlaceholderSecret(value string) bool {
	return placeholderSecrets[strings.ToLower(strings.TrimSpace(value))]
}
