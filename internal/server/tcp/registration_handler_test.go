package tcp

import (
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

func (m *getFailManager) RegisterOwned(conn *websocket.Conn, customSubdomain, remoteIP string, owner tunnel.Owner) (string, error) {
	return m.inner.RegisterOwned(conn, customSubdomain, remoteIP, owner)
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

	_, err = rh.Register(t.Context(), &RegistrationRequest{
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
