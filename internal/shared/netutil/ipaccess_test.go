package netutil

import (
	"strings"
	"testing"
)

func TestNewIPAccessCheckerRejectsInvalidRules(t *testing.T) {
	tests := []struct {
		name  string
		allow []string
		deny  []string
		want  string
	}{
		{
			name:  "invalid allow CIDR",
			allow: []string{"10.0.0.0/99"},
			want:  "allow IP/CIDR",
		},
		{
			name: "invalid deny IP",
			deny: []string{"999.999.999.999"},
			want: "deny IP/CIDR",
		},
		{
			name:  "empty entry",
			allow: []string{" "},
			want:  "must not be empty",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewIPAccessChecker(tt.allow, tt.deny)
			if err == nil {
				t.Fatal("NewIPAccessChecker expected error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("NewIPAccessChecker error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestNewIPAccessCheckerAllowsSingleIPAndCIDR(t *testing.T) {
	checker, err := NewIPAccessChecker([]string{"192.168.1.10", "10.0.0.0/8"}, []string{"10.0.0.5"})
	if err != nil {
		t.Fatalf("NewIPAccessChecker unexpected error: %v", err)
	}

	if !checker.IsAllowed("192.168.1.10") {
		t.Fatal("IsAllowed expected single allowed IP to pass")
	}
	if !checker.IsAllowed("10.1.2.3") {
		t.Fatal("IsAllowed expected allowed CIDR IP to pass")
	}
	if checker.IsAllowed("10.0.0.5") {
		t.Fatal("IsAllowed expected denied IP to fail")
	}
	if checker.IsAllowed("192.168.1.11") {
		t.Fatal("IsAllowed expected IP outside allow rules to fail")
	}
}
