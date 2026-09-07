package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestSaveConfigReplacesInsecurePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits")
	}
	for _, kind := range []string{"client", "server"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte("old"), 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0644); err != nil {
				t.Fatal(err)
			}
			var err error
			if kind == "client" {
				err = SaveClientConfig(&ClientConfig{Server: "example.test:443", Token: "private-token"}, path)
			} else {
				err = SaveServerConfig(&ServerConfig{AuthToken: "private-token"}, path)
			}
			if err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0600 {
				t.Fatalf("saved permissions = %o, want 600", info.Mode().Perm())
			}
		})
	}
}

func TestClientConfigRejectsNullTunnelAndInvalidPort(t *testing.T) {
	for _, cfg := range []*ClientConfig{
		{Server: "example.test:443", Tunnels: []*TunnelConfig{nil}},
		{Server: "example.test:65536"},
		{Server: "example.test:0"},
		{Server: "example.test:invalid"},
	} {
		if err := cfg.Validate(); err == nil {
			t.Errorf("Validate(%+v) succeeded", cfg)
		}
	}
	if err := (&ClientConfig{Server: "wss://[::1]:8443"}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestConfigRejectsMultipleYAMLDocuments(t *testing.T) {
	var cfg ClientConfig
	if err := decodeYAMLStrict([]byte("server: example.test:443\n---\nserver: other.test:443\n"), &cfg); err == nil {
		t.Fatal("ambiguous multi-document config was accepted")
	}
}
