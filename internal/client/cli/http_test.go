package cli

import (
	"strings"
	"testing"
)

func TestParseBandwidth(t *testing.T) {
	tests := []struct {
		input   string
		want    int64
		wantErr bool
	}{
		{"", 0, false},
		{"0", 0, false},
		{"1024", 1024, false},
		{"1K", 1024, false},
		{"1KB", 1024, false},
		{"1k", 1024, false},
		{"1M", 1024 * 1024, false},
		{"1MB", 1024 * 1024, false},
		{"1m", 1024 * 1024, false},
		{"10M", 10 * 1024 * 1024, false},
		{"1G", 1024 * 1024 * 1024, false},
		{"1GB", 1024 * 1024 * 1024, false},
		{"500K", 500 * 1024, false},
		{"100M", 100 * 1024 * 1024, false},
		{" 1M ", 1024 * 1024, false},
		{"1B", 1, false},
		{"100B", 100, false},
		{"invalid", 0, true},
		{"abc", 0, true},
		{"-1M", 0, true},
		{"-100", 0, true},
		{"1.5M", 0, true},
		{"M", 0, true},
		{"K", 0, true},
		{"9223372036854775807K", 0, true},
		{"9999999999999999999G", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := parseBandwidth(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Errorf("parseBandwidth(%q) = %d, want error", tt.input, got)
				}
				return
			}
			if err != nil {
				t.Errorf("parseBandwidth(%q) unexpected error: %v", tt.input, err)
				return
			}
			if got != tt.want {
				t.Errorf("parseBandwidth(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestRunHTTPRejectsInvalidIPAccessBeforeConfigResolution(t *testing.T) {
	oldAllowIPs := allowIPs
	oldDenyIPs := denyIPs
	oldDaemonMode := daemonMode
	oldDaemonMarker := daemonMarker
	oldAuthPass := authPass
	oldAuthBearer := authBearer
	t.Cleanup(func() {
		allowIPs = oldAllowIPs
		denyIPs = oldDenyIPs
		daemonMode = oldDaemonMode
		daemonMarker = oldDaemonMarker
		authPass = oldAuthPass
		authBearer = oldAuthBearer
	})

	allowIPs = []string{"not-a-cidr"}
	denyIPs = nil
	daemonMode = false
	daemonMarker = false
	authPass = ""
	authBearer = ""

	err := runHTTP(nil, []string{"3000"})
	if err == nil {
		t.Fatal("runHTTP expected error for invalid allow IP")
	}
	if !strings.Contains(err.Error(), "invalid IP access flags") {
		t.Fatalf("runHTTP error = %v, want invalid IP access flags", err)
	}
}
