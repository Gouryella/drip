package cli

import (
	"reflect"
	"strings"
	"testing"

	"drip/pkg/config"
)

func TestBuildServiceArgsBakesInEverythingTheServiceNeeds(t *testing.T) {
	t.Parallel()

	got := buildServiceArgs(serviceOptions{
		name:       "drip",
		configPath: `C:\ProgramData\drip\config.yaml`,
		logPath:    `C:\ProgramData\drip\logs\service.log`,
		tunnels:    []string{"web", "api"},
		verbose:    true,
	})

	want := []string{
		"service", "run",
		"--name", "drip",
		"--config", `C:\ProgramData\drip\config.yaml`,
		"--log", `C:\ProgramData\drip\logs\service.log`,
		"--tunnel", "web",
		"--tunnel", "api",
		"--verbose",
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildServiceArgs() = %#v, want %#v", got, want)
	}
}

func TestBuildServiceArgsWithAll(t *testing.T) {
	t.Parallel()

	got := buildServiceArgs(serviceOptions{
		name:       "drip",
		configPath: `C:\ProgramData\drip\config.yaml`,
		all:        true,
	})

	want := []string{
		"service", "run",
		"--name", "drip",
		"--config", `C:\ProgramData\drip\config.yaml`,
		"--all",
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildServiceArgs() = %#v, want %#v", got, want)
	}
}

func TestValidateServiceOptions(t *testing.T) {
	t.Parallel()

	valid := serviceOptions{name: "drip", all: true, startType: "delayed"}

	tests := []struct {
		name    string
		mutate  func(*serviceOptions)
		wantErr bool
	}{
		{name: "all tunnels", mutate: func(*serviceOptions) {}},
		{name: "named tunnels", mutate: func(o *serviceOptions) { o.all = false; o.tunnels = []string{"web"} }},
		{name: "auto start type", mutate: func(o *serviceOptions) { o.startType = "auto" }},
		{name: "manual start type", mutate: func(o *serviceOptions) { o.startType = "manual" }},
		{name: "no tunnel selected", mutate: func(o *serviceOptions) { o.all = false }, wantErr: true},
		{name: "all with named tunnel", mutate: func(o *serviceOptions) { o.tunnels = []string{"web"} }, wantErr: true},
		{name: "unknown start type", mutate: func(o *serviceOptions) { o.startType = "boot" }, wantErr: true},
		{name: "empty name", mutate: func(o *serviceOptions) { o.name = "" }, wantErr: true},
		{name: "name with slash", mutate: func(o *serviceOptions) { o.name = `drip\web` }, wantErr: true},
		{name: "password without username", mutate: func(o *serviceOptions) { o.password = "secret" }, wantErr: true},
		{name: "username with password", mutate: func(o *serviceOptions) { o.username = `.\drip`; o.password = "secret" }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			opts := valid
			tt.mutate(&opts)

			err := validateServiceOptions(opts)
			if tt.wantErr && err == nil {
				t.Fatalf("validateServiceOptions(%+v) expected error", opts)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("validateServiceOptions(%+v) unexpected error: %v", opts, err)
			}
		})
	}
}

func TestSelectTunnels(t *testing.T) {
	t.Parallel()

	cfg := &config.ClientConfig{
		Server: "tunnel.example.com:443",
		Tunnels: []*config.TunnelConfig{
			{Name: "web", Type: "http", Port: 3000},
			{Name: "api", Type: "http", Port: 8080},
		},
	}

	t.Run("all returns every tunnel", func(t *testing.T) {
		t.Parallel()

		got, err := selectTunnels(cfg, true, nil)
		if err != nil {
			t.Fatalf("selectTunnels() unexpected error: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("selectTunnels() returned %d tunnels, want 2", len(got))
		}
	})

	t.Run("names are resolved in order", func(t *testing.T) {
		t.Parallel()

		got, err := selectTunnels(cfg, false, []string{"api", "web"})
		if err != nil {
			t.Fatalf("selectTunnels() unexpected error: %v", err)
		}
		if len(got) != 2 || got[0].Name != "api" || got[1].Name != "web" {
			t.Fatalf("selectTunnels() = %v, want [api web]", got)
		}
	})

	t.Run("unknown name lists the available ones", func(t *testing.T) {
		t.Parallel()

		_, err := selectTunnels(cfg, false, []string{"db"})
		if err == nil {
			t.Fatal("selectTunnels() expected error for an unknown tunnel")
		}
		if !strings.Contains(err.Error(), "web") || !strings.Contains(err.Error(), "api") {
			t.Fatalf("selectTunnels() error = %q, want it to list the configured tunnels", err)
		}
	})

	t.Run("empty config is rejected", func(t *testing.T) {
		t.Parallel()

		_, err := selectTunnels(&config.ClientConfig{}, true, nil)
		if err == nil {
			t.Fatal("selectTunnels() expected error for a config without tunnels")
		}
	})
}

func TestIsUnderDir(t *testing.T) {
	t.Parallel()

	const dir = `C:\ProgramData\drip`

	tests := []struct {
		name string
		path string
		want bool
	}{
		{name: "the directory itself", path: dir, want: true},
		{name: "a file inside it", path: `C:\ProgramData\drip\config.yaml`, want: true},
		{name: "a nested file", path: `C:\ProgramData\drip\logs\service.log`, want: true},
		{name: "case insensitive", path: `c:\programdata\DRIP\config.yaml`, want: true},
		{name: "a sibling with a shared prefix", path: `C:\ProgramData\drip-backup\config.yaml`},
		{name: "a user profile", path: `C:\Users\me\.drip\config.yaml`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := isUnderDir(tt.path, dir); got != tt.want {
				t.Fatalf("isUnderDir(%q, %q) = %v, want %v", tt.path, dir, got, tt.want)
			}
		})
	}
}
