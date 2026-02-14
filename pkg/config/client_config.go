package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type TunnelConfig struct {
	Name      string   `yaml:"name"`                // Tunnel name (required, unique identifier)
	Type      string   `yaml:"type"`                // Tunnel type: http, https, tcp (required)
	Port      int      `yaml:"port"`                // Local port to forward (required)
	Address   string   `yaml:"address,omitempty"`   // Local address (default: 127.0.0.1)
	Subdomain string   `yaml:"subdomain,omitempty"` // Custom subdomain
	Transport string   `yaml:"transport,omitempty"` // Transport: auto, tcp, wss
	AllowIPs  []string `yaml:"allow_ips,omitempty"` // Allowed IPs/CIDRs
	DenyIPs   []string `yaml:"deny_ips,omitempty"`  // Denied IPs/CIDRs
	Auth      string   `yaml:"auth,omitempty"`      // Proxy authentication password (http/https only)
	Bandwidth string   `yaml:"bandwidth,omitempty"` // Bandwidth limit (e.g., 1M, 500K, 1G)
}

func (t *TunnelConfig) Validate() error {
	if t.Name == "" {
		return fmt.Errorf("tunnel name is required")
	}
	if t.Type == "" {
		return fmt.Errorf("tunnel type is required for '%s'", t.Name)
	}
	t.Type = strings.ToLower(t.Type)
	if t.Type != "http" && t.Type != "https" && t.Type != "tcp" {
		return fmt.Errorf("invalid tunnel type '%s' for '%s': must be http, https, or tcp", t.Type, t.Name)
	}
	if t.Port < 1 || t.Port > 65535 {
		return fmt.Errorf("invalid port %d for '%s': must be between 1 and 65535", t.Port, t.Name)
	}
	if t.Transport != "" {
		t.Transport = strings.ToLower(t.Transport)
		if t.Transport != "auto" && t.Transport != "tcp" && t.Transport != "wss" {
			return fmt.Errorf("invalid transport '%s' for '%s': must be auto, tcp, or wss", t.Transport, t.Name)
		}
	}
	return nil
}

type ClientConfig struct {
	Server  string          `yaml:"server"`            // Server address (e.g., tunnel.example.com:443)
	Token   string          `yaml:"token"`             // Authentication token
	TLS     bool            `yaml:"tls"`               // Use TLS (always true for production)
	Tunnels []*TunnelConfig `yaml:"tunnels,omitempty"` // Predefined tunnels
}

func (c *ClientConfig) Validate() error {
	if c.Server == "" {
		return fmt.Errorf("server address is required")
	}

	host, port, err := net.SplitHostPort(c.Server)
	if err != nil {
		if strings.Contains(err.Error(), "missing port") {
			return fmt.Errorf("server address must include port (e.g., example.com:443), got: %s", c.Server)
		}
		return fmt.Errorf("invalid server address format: %s (expected host:port)", c.Server)
	}

	if host == "" {
		return fmt.Errorf("server host is required")
	}

	if port == "" {
		return fmt.Errorf("server port is required")
	}

	// Validate tunnels and check for duplicate names
	names := make(map[string]bool)
	for _, t := range c.Tunnels {
		if err := t.Validate(); err != nil {
			return err
		}
		if names[t.Name] {
			return fmt.Errorf("duplicate tunnel name: %s", t.Name)
		}
		names[t.Name] = true
	}

	return nil
}

func (c *ClientConfig) GetTunnel(name string) *TunnelConfig {
	for _, t := range c.Tunnels {
		if t.Name == name {
			return t
		}
	}
	return nil
}

func (c *ClientConfig) GetTunnelNames() []string {
	names := make([]string, len(c.Tunnels))
	for i, t := range c.Tunnels {
		names[i] = t.Name
	}
	return names
}

func DefaultClientConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".drip/config.yaml"
	}
	return filepath.Join(home, ".drip", "config.yaml")
}

func LoadClientConfig(path string) (*ClientConfig, error) {
	if path == "" {
		path = DefaultClientConfigPath()
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("config file not found at %s, please run 'drip config init' first", path)
		}
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var config ClientConfig
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return &config, nil
}

func SaveClientConfig(config *ClientConfig, path string) error {
	if path == "" {
		path = DefaultClientConfigPath()
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

func ConfigExists(path string) bool {
	if path == "" {
		path = DefaultClientConfigPath()
	}
	_, err := os.Stat(path)
	return err == nil
}
