package proxy

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"drip/internal/server/tunnel"

	"go.uber.org/zap"
)

func newAdminRouteTestHandler(t *testing.T, cfg HandlerConfig) *Handler {
	t.Helper()

	if cfg.Logger == nil {
		cfg.Logger = zap.NewNop()
	}
	if cfg.Manager == nil {
		cfg.Manager = tunnel.NewManagerWithConfig(cfg.Logger, tunnel.ManagerConfig{
			MaxTunnels:      10,
			MaxTunnelsPerIP: 10,
			RateLimit:       1000,
			RateLimitWindow: time.Second,
		})
	}
	if cfg.ServerDomain == "" {
		cfg.ServerDomain = "drip.test"
	}
	if cfg.TunnelDomain == "" {
		cfg.TunnelDomain = "tunnels.test"
	}

	return NewHandler(cfg)
}

func TestStatsAreOnlyServedOnManagementHost(t *testing.T) {
	t.Parallel()

	handler := newAdminRouteTestHandler(t, HandlerConfig{
		MetricsToken: "metrics-secret",
	})

	req := httptest.NewRequest(http.MethodGet, "http://demo.tunnels.test/stats", nil)
	req.Header.Set("Authorization", "Bearer metrics-secret")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body = %q", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

func TestStatsPathOnTunnelHostIsForwardedToTunnel(t *testing.T) {
	server := newProxySSETestServer(t, func(conn net.Conn) {
		defer conn.Close()
		readProxyRequest(t, conn)
		_, _ = fmt.Fprint(conn,
			"HTTP/1.1 204 No Content\r\n"+
				"Content-Length: 0\r\n"+
				"\r\n")
	})
	defer server.Close()

	resp := doProxyRequestWithin(t, server, "/stats", time.Second)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusNoContent)
	}
}

func TestStatsAreServedOnServerHostWithMetricsToken(t *testing.T) {
	t.Parallel()

	handler := newAdminRouteTestHandler(t, HandlerConfig{
		MetricsToken: "metrics-secret",
	})

	req := httptest.NewRequest(http.MethodGet, "http://drip.test/stats", nil)
	req.Header.Set("Authorization", "Bearer metrics-secret")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", rec.Code, http.StatusOK, rec.Body.String())
	}
}

func TestMetricsRejectsEmptyTokenWithoutAuthFallback(t *testing.T) {
	t.Parallel()

	handler := newAdminRouteTestHandler(t, HandlerConfig{})

	req := httptest.NewRequest(http.MethodGet, "http://drip.test/metrics", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestMetricsFallsBackToAuthTokenWhenMetricsTokenIsEmpty(t *testing.T) {
	t.Parallel()

	handler := newAdminRouteTestHandler(t, HandlerConfig{
		AuthToken: "server-secret",
	})

	req := httptest.NewRequest(http.MethodGet, "http://drip.test/metrics", nil)
	req.Header.Set("Authorization", "Bearer server-secret")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %q", rec.Code, http.StatusOK, rec.Body.String())
	}
}
