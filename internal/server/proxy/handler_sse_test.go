package proxy

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"drip/internal/server/tunnel"
	"drip/internal/shared/protocol"
	"drip/pkg/config"

	"go.uber.org/zap"
)

const testTunnelDomain = "tunnels.test"

func newProxySSETestServer(t *testing.T, streamHandler func(net.Conn)) *httptest.Server {
	t.Helper()
	return newProxySSETestServerWithWriteTimeout(t, 0, streamHandler)
}

func newProxySSETestServerWithWriteTimeout(t *testing.T, writeTimeout time.Duration, streamHandler func(net.Conn)) *httptest.Server {
	t.Helper()
	return newProxySSETestServerWithHandlerConfig(t, writeTimeout, nil, streamHandler)
}

func newProxySSETestServerWithHandlerConfig(t *testing.T, writeTimeout time.Duration, configure func(*HandlerConfig), streamHandler func(net.Conn)) *httptest.Server {
	t.Helper()

	logger := zap.NewNop()
	manager := tunnel.NewManagerWithConfig(logger, tunnel.ManagerConfig{
		MaxTunnels:      10,
		MaxTunnelsPerIP: 10,
		RateLimit:       1000,
		RateLimitWindow: time.Second,
	})

	subdomain, err := manager.Register(nil, "demo")
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
	tconn.SetOpenStream(func() (net.Conn, error) {
		serverSide, clientSide := net.Pipe()
		go streamHandler(clientSide)
		return serverSide, nil
	})

	handlerConfig := HandlerConfig{
		Manager:      manager,
		Logger:       logger,
		ServerDomain: "drip.test",
		TunnelDomain: testTunnelDomain,
	}
	if configure != nil {
		configure(&handlerConfig)
	}
	handler := NewHandler(handlerConfig)

	server := httptest.NewUnstartedServer(handler)
	server.Config.WriteTimeout = writeTimeout
	server.Start()
	return server
}

func readProxyRequest(t *testing.T, conn net.Conn) {
	t.Helper()

	req, err := http.ReadRequest(bufio.NewReader(conn))
	if err != nil {
		t.Errorf("read proxied request: %v", err)
		return
	}
	_, _ = io.Copy(io.Discard, req.Body)
	_ = req.Body.Close()
}

func doProxyRequestWithin(t *testing.T, server *httptest.Server, path string, timeout time.Duration) *http.Response {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, server.URL+path, nil)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	req.Host = "demo." + testTunnelDomain

	respCh := make(chan *http.Response, 1)
	errCh := make(chan error, 1)
	go func() {
		resp, err := server.Client().Do(req)
		if err != nil {
			errCh <- err
			return
		}
		respCh <- resp
	}()

	select {
	case resp := <-respCh:
		return resp
	case err := <-errCh:
		t.Fatalf("proxy request failed: %v", err)
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for proxy response headers")
	}

	return nil
}

func readBodyWithin(t *testing.T, body io.Reader, size int, timeout time.Duration) string {
	t.Helper()

	type readResult struct {
		data []byte
		err  error
	}
	resultCh := make(chan readResult, 1)
	go func() {
		buf := make([]byte, size)
		_, err := io.ReadFull(body, buf)
		resultCh <- readResult{data: buf, err: err}
	}()

	select {
	case result := <-resultCh:
		if result.err != nil {
			t.Fatalf("read response body: %v", result.err)
		}
		return string(result.data)
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for response body")
	}

	return ""
}

func TestHandlerFlushesEventStreamResponse(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once

	server := newProxySSETestServer(t, func(conn net.Conn) {
		defer conn.Close()
		readProxyRequest(t, conn)
		_, _ = fmt.Fprint(conn,
			"HTTP/1.1 200 OK\r\n"+
				"Content-Type: text/event-stream; charset=utf-8\r\n"+
				"Content-Length: 999\r\n"+
				"Cache-Control: no-cache\r\n"+
				"\r\n"+
				"data: first\n\n")
		<-release
	})
	defer func() {
		releaseOnce.Do(func() { close(release) })
		server.Close()
	}()

	resp := doProxyRequestWithin(t, server, "/events", time.Second)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if got := resp.Header.Get("Content-Length"); got != "" {
		t.Fatalf("Content-Length = %q, want empty for SSE", got)
	}

	got := readBodyWithin(t, resp.Body, len("data: first\n\n"), 500*time.Millisecond)
	if got != "data: first\n\n" {
		t.Fatalf("body prefix = %q, want first SSE event", got)
	}

	releaseOnce.Do(func() { close(release) })
}

func TestHandlerRefreshesWriteDeadlineForSlowEventStream(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once

	server := newProxySSETestServerWithWriteTimeout(t, 100*time.Millisecond, func(conn net.Conn) {
		defer conn.Close()
		readProxyRequest(t, conn)
		_, _ = fmt.Fprint(conn,
			"HTTP/1.1 200 OK\r\n"+
				"Content-Type: text/event-stream\r\n"+
				"Cache-Control: no-cache\r\n"+
				"\r\n")
		time.Sleep(250 * time.Millisecond)
		_, _ = fmt.Fprint(conn, "data: delayed\n\n")
		<-release
	})
	defer func() {
		releaseOnce.Do(func() { close(release) })
		server.Close()
	}()

	resp := doProxyRequestWithin(t, server, "/events", time.Second)
	defer resp.Body.Close()

	got := readBodyWithin(t, resp.Body, len("data: delayed\n\n"), time.Second)
	if got != "data: delayed\n\n" {
		t.Fatalf("body prefix = %q, want delayed SSE event", got)
	}

	releaseOnce.Do(func() { close(release) })
}

func TestHandlerRejectsOversizedTunnelResponseHeader(t *testing.T) {
	server := newProxySSETestServer(t, func(conn net.Conn) {
		defer conn.Close()
		readProxyRequest(t, conn)
		_, _ = fmt.Fprintf(conn,
			"HTTP/1.1 200 OK\r\n"+
				"X-Large: %s\r\n"+
				"\r\n"+
				"hello",
			strings.Repeat("a", int(defaultMaxResponseHeaderBytes)+1024),
		)
	})
	defer server.Close()

	resp := doProxyRequestWithin(t, server, "/large-header?secret=hidden", time.Second)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusBadGateway)
	}
}

func TestHandlerTimesOutReadingTunnelResponseHeader(t *testing.T) {
	server := newProxySSETestServerWithHandlerConfig(t, 0, func(cfg *HandlerConfig) {
		cfg.ResponseHeaderTimeout = 20 * time.Millisecond
	}, func(conn net.Conn) {
		defer conn.Close()
		readProxyRequest(t, conn)
		time.Sleep(200 * time.Millisecond)
	})
	defer server.Close()

	resp := doProxyRequestWithin(t, server, "/slow-header", time.Second)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusBadGateway)
	}
}

func TestHandlerRejectsRequestBodyOverDefaultLimitBeforeOpeningStream(t *testing.T) {
	logger := zap.NewNop()
	manager := tunnel.NewManagerWithConfig(logger, tunnel.ManagerConfig{
		MaxTunnels:      10,
		MaxTunnelsPerIP: 10,
		RateLimit:       1000,
		RateLimitWindow: time.Second,
	})

	subdomain, err := manager.Register(nil, "demo")
	if err != nil {
		t.Fatalf("register tunnel: %v", err)
	}
	defer manager.Unregister(subdomain)

	tconn, ok := manager.Get(subdomain)
	if !ok {
		t.Fatalf("registered tunnel %q was not found", subdomain)
	}
	tconn.SetTunnelType(protocol.TunnelTypeHTTP)
	streamOpened := false
	tconn.SetOpenStream(func() (net.Conn, error) {
		streamOpened = true
		return nil, fmt.Errorf("stream should not be opened")
	})

	handler := NewHandler(HandlerConfig{
		Manager:      manager,
		Logger:       logger,
		ServerDomain: "drip.test",
		TunnelDomain: testTunnelDomain,
	})

	req := httptest.NewRequest(http.MethodPost, "http://demo."+testTunnelDomain+"/upload?secret=hidden", http.NoBody)
	req.Host = "demo." + testTunnelDomain
	req.ContentLength = config.DefaultMaxRequestBodyBytes + 1
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusRequestEntityTooLarge)
	}
	if streamOpened {
		t.Fatal("stream was opened for an over-limit request body")
	}
}

func TestHandlerPreservesContentLengthForOrdinaryResponse(t *testing.T) {
	server := newProxySSETestServer(t, func(conn net.Conn) {
		defer conn.Close()
		readProxyRequest(t, conn)
		_, _ = fmt.Fprint(conn,
			"HTTP/1.1 200 OK\r\n"+
				"Content-Type: text/plain\r\n"+
				"Content-Length: 5\r\n"+
				"\r\n"+
				"hello")
	})
	defer server.Close()

	resp := doProxyRequestWithin(t, server, "/plain", time.Second)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read ordinary response body: %v", err)
	}
	if got := resp.Header.Get("Content-Length"); got != "5" {
		t.Fatalf("Content-Length = %q, want 5", got)
	}
	if string(body) != "hello" {
		t.Fatalf("body = %q, want hello", body)
	}
}
