package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"drip/internal/server/tunnel"
	"drip/internal/shared/protocol"
	"go.uber.org/zap"
)

// newIPACLHandler builds a handler serving one HTTP tunnel with IP access rules.
func newIPACLHandler(t *testing.T, subdomain string, trustedProxies, allowIPs, denyIPs []string) *Handler {
	t.Helper()

	manager := tunnel.NewManager(zap.NewNop())
	t.Cleanup(manager.Shutdown)

	if _, err := manager.Register(nil, subdomain); err != nil {
		t.Fatalf("register tunnel: %v", err)
	}
	conn, ok := manager.Get(subdomain)
	if !ok {
		t.Fatal("tunnel was not registered")
	}
	t.Cleanup(conn.Close)
	conn.SetTunnelType(protocol.TunnelTypeHTTP)
	if err := conn.SetIPAccessControl(allowIPs, denyIPs); err != nil {
		t.Fatalf("set IP access control: %v", err)
	}

	h := NewHandler(HandlerConfig{
		Manager:        manager,
		ServerDomain:   "connect.example.test",
		TunnelDomain:   "example.test",
		TrustedProxies: trustedProxies,
	})
	return h
}

// serveTunnelRequest routes one request through the handler and returns the status.
// A tunnel with IP access rules and no backend stream answers 403 for a rejected
// client and 502 for an accepted one, so those two codes distinguish allow/deny.
func serveTunnelRequest(h *Handler, subdomain, peer, xForwardedFor string) int {
	req := httptest.NewRequest(http.MethodGet, "https://"+subdomain+".example.test/", nil)
	req.RemoteAddr = peer
	if xForwardedFor != "" {
		req.Header.Set("X-Forwarded-For", xForwardedFor)
	}
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, req)
	return recorder.Code
}

// A client that is allowed by the tunnel ACL is proxied (502: no backend stream),
// a client that is not allowed is rejected (403).
func TestSpoofedXForwardedForCannotSatisfyTunnelAllowList(t *testing.T) {
	for _, tc := range []struct {
		name           string
		trustedProxies []string
		peer           string
		xff            string
		want           int
	}{
		{"trusted proxy ignores attacker-supplied leading entry", []string{"10.0.0.0/8"}, "10.0.0.1:1234", "203.0.113.77, 198.51.100.44", http.StatusForbidden},
		{"untrusted peer cannot spoof the header at all", []string{"10.0.0.0/8"}, "198.51.100.50:1234", "203.0.113.77", http.StatusForbidden},
		{"no trusted proxies configured ignores the header", nil, "10.0.0.1:1234", "203.0.113.77", http.StatusForbidden},
		{"real client that is allowed is proxied", []string{"10.0.0.0/8"}, "10.0.0.1:1234", "203.0.113.77", http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newIPACLHandler(t, "acl-allow", tc.trustedProxies, []string{"203.0.113.77"}, nil)
			if got := serveTunnelRequest(h, "acl-allow", tc.peer, tc.xff); got != tc.want {
				t.Fatalf("status = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestSpoofedXForwardedForCannotEvadeTunnelDenyList(t *testing.T) {
	h := newIPACLHandler(t, "acl-deny", []string{"10.0.0.0/8"}, nil, []string{"198.51.100.44"})

	if got := serveTunnelRequest(h, "acl-deny", "10.0.0.1:1234", "203.0.113.77, 198.51.100.44"); got != http.StatusForbidden {
		t.Fatalf("denied client hid behind a spoofed entry: status = %d, want %d", got, http.StatusForbidden)
	}
	if got := serveTunnelRequest(h, "acl-deny", "10.0.0.1:1234", "203.0.113.77, 203.0.113.76"); got != http.StatusBadGateway {
		t.Fatalf("unrelated client was denied: status = %d, want %d", got, http.StatusBadGateway)
	}
}

// Repeated failures from one real client must fill one rate-limit bucket even when
// the attacker rotates the forwarded value on every attempt.
func TestSpoofedXForwardedForCannotChooseProxyAuthRateLimitBucket(t *testing.T) {
	const (
		subdomain  = "acl-ratelimit"
		realClient = "198.51.100.44"
	)

	manager := tunnel.NewManager(zap.NewNop())
	t.Cleanup(manager.Shutdown)
	if _, err := manager.Register(nil, subdomain); err != nil {
		t.Fatalf("register tunnel: %v", err)
	}
	conn, ok := manager.Get(subdomain)
	if !ok {
		t.Fatal("tunnel was not registered")
	}
	t.Cleanup(conn.Close)
	conn.SetTunnelType(protocol.TunnelTypeHTTP)
	conn.SetProxyAuth(&protocol.ProxyAuth{Enabled: true, Type: "bearer", Token: "tunnel-token"})

	h := NewHandler(HandlerConfig{
		Manager:        manager,
		ServerDomain:   "connect.example.test",
		TunnelDomain:   "example.test",
		TrustedProxies: []string{"10.0.0.0/8"},
	})

	key := authRateLimitKey(realClient, subdomain)
	authLimiter.resetFailures(key)
	t.Cleanup(func() { authLimiter.resetFailures(key) })

	attempt := func(spoofedIP string) int {
		req := httptest.NewRequest(http.MethodGet, "https://"+subdomain+".example.test/", nil)
		req.RemoteAddr = "10.0.0.1:1234"
		req.Header.Set("Authorization", "Bearer wrong-token")
		req.Header.Set("X-Forwarded-For", spoofedIP+", "+realClient)
		recorder := httptest.NewRecorder()
		h.ServeHTTP(recorder, req)
		return recorder.Code
	}

	for i := 0; i < authRateLimitMax; i++ {
		if got := attempt(fmt.Sprintf("203.0.113.%d", i+1)); got != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status = %d, want %d", i, got, http.StatusUnauthorized)
		}
	}
	if got := attempt("203.0.113.200"); got != http.StatusTooManyRequests {
		t.Fatalf("rotating the forwarded IP reset the rate-limit bucket: status = %d, want %d", got, http.StatusTooManyRequests)
	}
}
