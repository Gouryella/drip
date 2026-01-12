package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// ClientConfig represents the client configuration
type ClientConfig struct {
	Server string `yaml:"server"` // Server address (e.g., tunnel.example.com:443)
	Token  string `yaml:"token"`  // Authentication token
	TLS    bool   `yaml:"tls"`    // Use TLS (always true for production)
}

// Validate checks if the client configuration is valid
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

	return nil
}

// DefaultClientConfig returns the default configuration path
func DefaultClientConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".drip/config.yaml"
	}
	return filepath.Join(home, ".drip", "config.yaml")
}

// LoadClientConfig loads configuration from file
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

// SaveClientConfig saves configuration to file
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

	// Write to file with secure permissions
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}

	return nil
}

// ConfigExists checks if config file exists
func ConfigExists(path string) bool {
	if path == "" {
		path = DefaultClientConfigPath()
	}
	_, err := os.Stat(path)
	return err == nil
}
