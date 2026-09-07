package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"drip/internal/server/metrics"
	"drip/internal/server/tunnel"
	"drip/internal/shared/protocol"
)

const authObservabilityTunnelDomain = "auth-tunnels.test"

func TestProxyBearerAuthFailureLogsRedactedAndIncrementsCounter(t *testing.T) {
	handler, observed, subdomain := newAuthObservabilityHandler(t, &protocol.ProxyAuth{
		Enabled: true,
		Type:    "bearer",
		Token:   "super-secret-token",
	})

	clientIP := "203.0.113.44"
	authLimiter.resetFailures(authRateLimitKey(clientIP, subdomain))
	t.Cleanup(func() {
		authLimiter.resetFailures(authRateLimitKey(clientIP, subdomain))
	})

	counter := metrics.ProxyAuthEvents.WithLabelValues(proxyAuthTypeBearer, proxyAuthOutcomeFailure, proxyAuthReasonMissingToken)
	before := counterValue(t, counter)

	req := httptest.NewRequest(http.MethodGet, "https://"+subdomain+"."+authObservabilityTunnelDomain+"/private?token=query-secret&visible=plain", nil)
	req.Host = subdomain + "." + authObservabilityTunnelDomain
	req.RemoteAddr = clientIP + ":54321"
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusUnauthorized)
	}
	if got := counterValue(t, counter) - before; got != 1 {
		t.Fatalf("auth failure counter delta = %v, want 1", got)
	}

	entries := observed.FilterMessage("Proxy authentication failed").All()
	if len(entries) != 1 {
		t.Fatalf("got %d auth failure log entries, want 1", len(entries))
	}
	assertAuthLogRedacted(t, entries[0].ContextMap(), clientIP, subdomain, "query-secret", "super-secret-token")
}

func TestProxyAuthLockoutLogsRedactedAndIncrementsCounter(t *testing.T) {
	handler, observed, subdomain := newAuthObservabilityHandler(t, &protocol.ProxyAuth{
		Enabled:  true,
		Password: "correct-password",
	})

	clientIP := "203.0.113.45"
	authLimiter.mu.Lock()
	authLimiter.entries[authRateLimitKey(clientIP, subdomain)] = &authRateLimitEntry{
		failures:    authRateLimitMax,
		windowStart: time.Now(),
	}
	authLimiter.mu.Unlock()
	t.Cleanup(func() {
		authLimiter.resetFailures(authRateLimitKey(clientIP, subdomain))
	})

	counter := metrics.ProxyAuthEvents.WithLabelValues(proxyAuthTypePassword, proxyAuthOutcomeLockout, proxyAuthReasonRateLimited)
	before := counterValue(t, counter)

	req := httptest.NewRequest(http.MethodGet, "https://"+subdomain+"."+authObservabilityTunnelDomain+"/private?password=query-secret", nil)
	req.Host = subdomain + "." + authObservabilityTunnelDomain
	req.RemoteAddr = clientIP + ":54321"
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusTooManyRequests)
	}
	if got := counterValue(t, counter) - before; got != 1 {
		t.Fatalf("auth lockout counter delta = %v, want 1", got)
	}

	entries := observed.FilterMessage("Proxy authentication lockout").All()
	if len(entries) != 1 {
		t.Fatalf("got %d auth lockout log entries, want 1", len(entries))
	}
	assertAuthLogRedacted(t, entries[0].ContextMap(), clientIP, subdomain, "query-secret", "correct-password")
}

func newAuthObservabilityHandler(t *testing.T, auth *protocol.ProxyAuth) (*Handler, *observer.ObservedLogs, string) {
	t.Helper()

	core, observed := observer.New(zap.DebugLevel)
	logger := zap.New(core)
	manager := tunnel.NewManagerWithConfig(logger, tunnel.ManagerConfig{
		MaxTunnels:      10,
		MaxTunnelsPerIP: 10,
		RateLimit:       1000,
		RateLimitWindow: time.Second,
	})

	subdomain, err := manager.Register(nil, "authdemo")
	if err != nil {
		t.Fatalf("register tunnel: %v", err)
	}
	t.Cleanup(func() {
		manager.Unregister(subdomain)
	})

	tconn, ok := manager.Get(subdomain)
	if !ok {
		t.Fatalf("registered tunnel %q was not found", subdomain)
	}
	tconn.SetTunnelType(protocol.TunnelTypeHTTP)
	tconn.SetProxyAuth(auth)

	handler := NewHandler(HandlerConfig{
		Manager:      manager,
		Logger:       logger,
		ServerDomain: "drip.test",
		TunnelDomain: authObservabilityTunnelDomain,
	})
	return handler, observed, subdomain
}

func assertAuthLogRedacted(t *testing.T, fields map[string]interface{}, clientIP, subdomain string, secrets ...string) {
	t.Helper()

	for _, forbiddenKey := range []string{"client_ip", "ip", "subdomain", "token", "password"} {
		if _, ok := fields[forbiddenKey]; ok {
			t.Fatalf("auth log contains forbidden raw field %q: %#v", forbiddenKey, fields)
		}
	}
	if fields["client_ip_hash"] == clientIP {
		t.Fatalf("client_ip_hash contains raw client IP: %#v", fields)
	}
	if fields["subdomain_hash"] == subdomain {
		t.Fatalf("subdomain_hash contains raw subdomain: %#v", fields)
	}

	rendered := fmt.Sprint(fields)
	for _, secret := range append([]string{clientIP, subdomain}, secrets...) {
		if secret != "" && strings.Contains(rendered, secret) {
			t.Fatalf("auth log leaked %q in fields: %s", secret, rendered)
		}
	}
}

func counterValue(t *testing.T, counter prometheus.Counter) float64 {
	t.Helper()

	var metric dto.Metric
	if err := counter.Write(&metric); err != nil {
		t.Fatalf("read counter: %v", err)
	}
	return metric.GetCounter().GetValue()
}
