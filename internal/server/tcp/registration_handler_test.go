package tcp

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"drip/internal/server/tunnel"
	"drip/internal/shared/protocol"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

// getFailManager registers successfully but makes Get miss so Register must roll back.
type getFailManager struct {
	inner *tunnel.Manager
}

func (m *getFailManager) RegisterWithIP(conn *websocket.Conn, customSubdomain, remoteIP string) (string, error) {
	return m.inner.RegisterWithIP(conn, customSubdomain, remoteIP)
}

func (m *getFailManager) Get(subdomain string) (*tunnel.Connection, bool) {
	return nil, false
}

func (m *getFailManager) Unregister(subdomain string) {
	m.inner.Unregister(subdomain)
}

func TestRegisterReleasesResourcesWhenTunnelLookupFails(t *testing.T) {
	t.Parallel()

	logger := zap.NewNop()
	base := tunnel.NewManagerWithConfig(logger, tunnel.ManagerConfig{
		MaxTunnels:      10,
		MaxTunnelsPerIP: 10,
		RateLimit:       1000,
		RateLimitWindow: time.Second,
	})

	portAlloc, err := NewPortAllocator(30000, 30010)
	if err != nil {
		t.Fatalf("create port allocator: %v", err)
	}

	rh := NewRegistrationHandler(
		&getFailManager{inner: base},
		portAlloc,
		nil,
		"example.com",
		"tunnels.example.com",
		443,
		logger,
	)

	_, err = rh.Register(&RegistrationRequest{
		TunnelType:      protocol.TunnelTypeTCP,
		CustomSubdomain: "tcp-30001",
		RemoteIP:        "203.0.113.10",
	})
	if err == nil {
		t.Fatal("expected registration to fail when tunnel lookup fails")
	}

	// Port must be released so a subsequent specific allocation succeeds.
	port, allocErr := portAlloc.AllocateSpecific(30001)
	if allocErr != nil {
		t.Fatalf("expected port 30001 to be free after failed registration, got: %v", allocErr)
	}
	portAlloc.Release(port)

	if base.Count() != 0 {
		t.Fatalf("expected tunnel manager to be empty after rollback, got count=%d", base.Count())
	}
}

func TestPublicRegistrationErrorDoesNotExposeInternalError(t *testing.T) {
	internalErr := fmt.Errorf("tunnel registration failed for secret-subdomain: %w", tunnel.ErrSubdomainTaken)

	code, message := publicRegistrationError(internalErr)
	if code != publicRegistrationFailureCode {
		t.Fatalf("code = %q, want %q", code, publicRegistrationFailureCode)
	}
	if message != publicRegistrationFailureMessage {
		t.Fatalf("message = %q, want %q", message, publicRegistrationFailureMessage)
	}
	if message == internalErr.Error() {
		t.Fatal("public message reused internal error text")
	}
	if containsAny(message, "secret-subdomain", "already taken", "tunnel registration failed for") {
		t.Fatalf("public message leaked internal error text: %q", message)
	}
}

func TestRegistrationFailureReasonMapsKnownErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "subdomain taken",
			err:  fmt.Errorf("tunnel registration failed: %w", tunnel.ErrSubdomainTaken),
			want: "subdomain_taken",
		},
		{
			name: "rate limited",
			err:  fmt.Errorf("tunnel registration failed: %w", tunnel.ErrRateLimitExceeded),
			want: "rate_limited",
		},
		{
			name: "port allocation",
			err:  fmt.Errorf("failed to allocate port: no ports available"),
			want: "port_allocation",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := registrationFailureReason(tt.err); got != tt.want {
				t.Fatalf("registrationFailureReason() = %q, want %q", got, tt.want)
			}
		})
	}
}

func containsAny(s string, needles ...string) bool {
	for _, needle := range needles {
		if needle != "" && strings.Contains(s, needle) {
			return true
		}
	}
	return false
}
