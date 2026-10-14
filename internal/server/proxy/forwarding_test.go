package proxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"drip/internal/server/tunnel"
	"drip/internal/shared/protocol"
	"go.uber.org/zap"
)

func TestForwardedRequestUsesTrustedPeerMetadata(t *testing.T) {
	h := NewHandler(HandlerConfig{TrustedProxies: []string{"10.0.0.0/8"}})
	for _, tc := range []struct{ peer, wantIP, wantProto string }{
		{"203.0.113.1:1234", "203.0.113.1", "http"},
		{"10.0.0.1:1234", "198.51.100.2", "https"},
	} {
		req := httptest.NewRequest(http.MethodGet, "http://demo.example.test/", nil)
		req.RemoteAddr = tc.peer
		req.Header.Set("X-Forwarded-For", "198.51.100.2, 10.0.0.2")
		req.Header.Set("X-Real-IP", "127.0.0.1")
		req.Header.Set("X-Forwarded-Proto", "https")
		req.Header.Set("Forwarded", "for=127.0.0.1")
		req.Header.Add("Connection", "X-Internal")
		req.Header.Set("X-Internal", "private")
		out := h.forwardedRequest(req)
		if out.Header.Get("X-Forwarded-For") != tc.wantIP || out.Header.Get("X-Real-IP") != tc.wantIP || out.Header.Get("X-Forwarded-Proto") != tc.wantProto {
			t.Errorf("peer %s: forwarded headers = %v", tc.peer, out.Header)
		}
		if out.Header.Get("Forwarded") != "" || out.Header.Get("X-Internal") != "" {
			t.Error("untrusted/hop headers were forwarded")
		}
		if req.Header.Get("X-Internal") != "private" {
			t.Error("original request was mutated")
		}
	}
}

func TestResponseHeadersStripAllConnectionTokens(t *testing.T) {
	h := NewHandler(HandlerConfig{})
	src := http.Header{"Connection": {"X-First", "X-Second"}, "X-First": {"a"}, "X-Second": {"b"}, "Proxy-Authenticate": {"secret"}, "Content-Type": {"text/plain"}}
	dst := make(http.Header)
	h.copyResponseHeaders(dst, src, "demo.example.test")
	if len(dst) != 1 || dst.Get("Content-Type") != "text/plain" {
		t.Fatalf("forwarded response headers = %v", dst)
	}
}

func TestRewriteLocationPreservesEscapedPath(t *testing.T) {
	h := NewHandler(HandlerConfig{})
	got := h.rewriteLocationHeader("http://[::1]:3000/a%2Fb?q=%2F#part", "demo.example.test")
	if want := "https://demo.example.test/a%2Fb?q=%2F#part"; got != want {
		t.Fatalf("Location = %q, want %q", got, want)
	}
}

func TestCanceledRequestClosesStreamWhileWaitingForHeaders(t *testing.T) {
	started, closed := make(chan struct{}), make(chan struct{})
	server := newProxySSETestServer(t, func(conn net.Conn) {
		defer conn.Close()
		readProxyRequest(t, conn)
		close(started)
		_, _ = io.Copy(io.Discard, conn)
		close(closed)
	})
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	req.Host = "demo." + testTunnelDomain
	done := make(chan struct{})
	go func() {
		resp, _ := server.Client().Do(req)
		if resp != nil {
			resp.Body.Close()
		}
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("request was not forwarded")
	}
	cancel()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("cancellation did not interrupt response header read")
	}
	<-done
}

func TestIdleHTTP2EventStreamOutlivesWriteTimeout(t *testing.T) {
	release := make(chan struct{})
	base := newProxySSETestServerWithHandlerConfig(t, 0, func(cfg *HandlerConfig) { cfg.StreamingWriteTimeout = 20 * time.Millisecond }, func(conn net.Conn) {
		defer conn.Close()
		readProxyRequest(t, conn)
		_, _ = fmt.Fprint(conn, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\n\r\n")
		time.Sleep(80 * time.Millisecond)
		_, _ = fmt.Fprint(conn, "data: alive\n\n")
		<-release
	})
	defer base.Close()
	server := httptest.NewUnstartedServer(base.Config.Handler)
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	defer close(release)
	resp := doProxyRequestWithin(t, server, "/events", time.Second)
	defer resp.Body.Close()
	if resp.ProtoMajor != 2 {
		t.Fatalf("protocol = %s", resp.Proto)
	}
	if got := readBodyWithin(t, resp.Body, len("data: alive\n\n"), time.Second); got != "data: alive\n\n" {
		t.Fatalf("event = %q", got)
	}
}

func TestAuthCookieDoesNotSurviveTunnelReplacementOrPasswordChange(t *testing.T) {
	h := NewHandler(HandlerConfig{})
	original := tunnel.NewConnection("demo", nil, zap.NewNop())
	defer original.Close()
	original.SetProxyAuth(&protocol.ProxyAuth{Enabled: true, Password: "old-password"})
	token := sessionStore.create("demo", original.ProxyAuthID())
	t.Cleanup(func() { sessionStore.mu.Lock(); sessionStore.removeSessionLocked(token); sessionStore.mu.Unlock() })
	req := httptest.NewRequest(http.MethodGet, "https://demo.example.test/", nil)
	req.AddCookie(&http.Cookie{Name: authCookieName + "_demo", Value: token})
	if !h.isProxyAuthenticated(req, "demo", original) {
		t.Fatal("fresh cookie was rejected")
	}
	replacement := tunnel.NewConnection("demo", nil, zap.NewNop())
	defer replacement.Close()
	replacement.SetProxyAuth(&protocol.ProxyAuth{Enabled: true, Password: "new-password"})
	if h.isProxyAuthenticated(req, "demo", replacement) {
		t.Fatal("old cookie authorized a replacement tunnel")
	}
	original.SetProxyAuth(&protocol.ProxyAuth{Enabled: true, Password: "new-password"})
	if h.isProxyAuthenticated(req, "demo", original) {
		t.Fatal("old cookie survived password rotation")
	}
}

func TestSuccessfulAuthOnOtherTunnelDoesNotResetFailureLimit(t *testing.T) {
	manager := tunnel.NewManager(zap.NewNop())
	defer manager.Shutdown()
	clientIP := "192.0.2.99"
	for _, name := range []string{"victim", "owned"} {
		if _, err := manager.Register(nil, name); err != nil {
			t.Fatal(err)
		}
		conn, _ := manager.Get(name)
		conn.SetTunnelType(protocol.TunnelTypeHTTP)
		conn.SetProxyAuth(&protocol.ProxyAuth{Enabled: true, Type: "bearer", Token: name + "-token"})
		key := authRateLimitKey(clientIP, name)
		authLimiter.resetFailures(key)
		t.Cleanup(func() { authLimiter.resetFailures(key) })
	}
	h := NewHandler(HandlerConfig{Manager: manager, ServerDomain: "connect.example.test", TunnelDomain: "example.test"})
	request := func(name, token string) int {
		req := httptest.NewRequest(http.MethodGet, "https://"+name+".example.test/", nil)
		req.RemoteAddr = clientIP + ":1234"
		req.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		h.ServeHTTP(response, req)
		return response.Code
	}
	for i := 0; i < authRateLimitMax; i++ {
		if got := request("victim", "wrong-token"); got != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status %d", i, got)
		}
		// Authentication succeeds; this synthetic tunnel has no backend stream.
		if got := request("owned", "owned-token"); got != http.StatusBadGateway {
			t.Fatalf("own tunnel status = %d", got)
		}
	}
	if got := request("victim", "victim-token"); got != http.StatusTooManyRequests {
		t.Fatalf("other tunnel reset the victim's failure limit: status %d", got)
	}
}
