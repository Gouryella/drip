package config

import (
	"os"
	"path/filepath"
)

// Replace atomically so interruption cannot leave half a configuration, and
// saving over an existing world-readable file cannot expose new credentials.
func writeConfigFile(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".drip-config-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
