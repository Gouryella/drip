package tcp

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"drip/internal/shared/protocol"
	"drip/internal/shared/stats"

	"go.uber.org/zap"
)

func TestHandleHTTPStreamCancelsPostEventStreamWhenTunnelCloses(t *testing.T) {
	backendCanceled := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: ready\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(backendCanceled)
	}))
	defer backend.Close()
	host, portText, _ := net.SplitHostPort(backend.Listener.Addr().String())
	port, _ := strconv.Atoi(portText)
	c := NewPoolClient(&ConnectorConfig{ServerAddr: "localhost:443", TunnelType: protocol.TunnelTypeHTTP, LocalHost: host, LocalPort: port}, zap.NewNop())
	defer c.Close()
	serverSide, clientSide := net.Pipe()
	defer serverSide.Close()
	defer clientSide.Close()
	done := make(chan struct{})
	go func() { defer close(done); c.handleHTTPStream(clientSide) }()
	req, _ := http.NewRequest(http.MethodPost, "http://demo.test/events", strings.NewReader("request-body"))
	if err := req.Write(serverSide); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(serverSide), req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := readClientBodyWithin(t, resp.Body, len("data: ready\n\n"), time.Second); got != "data: ready\n\n" {
		t.Fatalf("event = %q", got)
	}
	_ = serverSide.Close()
	select {
	case <-backendCanceled:
	case <-time.After(time.Second):
		t.Fatal("POST event stream did not cancel its backend request")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("POST handler did not stop")
	}
}

type recordingConn struct {
	net.Conn
	mu            sync.Mutex
	writeDeadline time.Time
}

func (c *recordingConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	c.writeDeadline = t
	c.mu.Unlock()
	return c.Conn.SetWriteDeadline(t)
}

func (c *recordingConn) lastWriteDeadline() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writeDeadline
}

func TestHandleHTTPStreamForwardsEventStreamWithStreamingWriteDeadline(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/events" {
			t.Errorf("path = %q, want /events", r.URL.Path)
			http.NotFound(w, r)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("backend ResponseWriter does not support flush")
			return
		}

		_, _ = fmt.Fprint(w, "data: first\n\n")
		flusher.Flush()
		<-release
	}))
	defer func() {
		releaseOnce.Do(func() { close(release) })
		backend.Close()
	}()

	backendURL, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatalf("parse backend URL: %v", err)
	}
	localHost, localPortText, err := net.SplitHostPort(backendURL.Host)
	if err != nil {
		t.Fatalf("split backend host: %v", err)
	}
	localPort, err := strconv.Atoi(localPortText)
	if err != nil {
		t.Fatalf("parse backend port: %v", err)
	}

	poolClient := &PoolClient{
		tunnelType: protocol.TunnelTypeHTTP,
		localHost:  localHost,
		localPort:  localPort,
		httpClient: newLocalHTTPClient(protocol.TunnelTypeHTTP, false),
		ctx:        context.Background(),
		stats:      stats.NewTrafficStats(),
		logger:     zap.NewNop(),
	}

	serverSide, rawClientSide := net.Pipe()
	defer serverSide.Close()
	clientSide := &recordingConn{Conn: rawClientSide}

	done := make(chan struct{})
	go func() {
		defer close(done)
		poolClient.handleHTTPStream(clientSide)
	}()

	req, err := http.NewRequest(http.MethodGet, "http://demo.tunnels.test/events", nil)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	if err := req.Write(serverSide); err != nil {
		t.Fatalf("write request to stream: %v", err)
	}

	resp, err := http.ReadResponse(bufio.NewReader(serverSide), req)
	if err != nil {
		t.Fatalf("read response from stream: %v", err)
	}
	defer resp.Body.Close()

	got := readClientBodyWithin(t, resp.Body, len("data: first\n\n"), time.Second)
	if got != "data: first\n\n" {
		t.Fatalf("body prefix = %q, want first SSE event", got)
	}

	if deadline := clientSide.lastWriteDeadline(); deadline.IsZero() {
		t.Fatal("SSE stream write deadline is zero, want bounded deadline")
	} else if time.Until(deadline) <= 0 {
		t.Fatalf("SSE stream write deadline = %v, want future deadline", deadline)
	}

	releaseOnce.Do(func() { close(release) })
	_ = serverSide.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("handleHTTPStream did not return after stream close")
	}
}

func TestLocalHTTPClientLimitsResponseHeaders(t *testing.T) {
	client := newLocalHTTPClient(protocol.TunnelTypeHTTP, false)
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport = %T, want *http.Transport", client.Transport)
	}
	if transport.MaxResponseHeaderBytes != maxLocalResponseHeaderBytes {
		t.Fatalf("MaxResponseHeaderBytes = %d, want %d", transport.MaxResponseHeaderBytes, maxLocalResponseHeaderBytes)
	}
}

func TestHandleHTTPStreamCancelsIdleEventStreamWhenTunnelCloses(t *testing.T) {
	backendCanceled := make(chan struct{})

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("backend ResponseWriter does not support flush")
			return
		}

		_, _ = fmt.Fprint(w, "data: first\n\n")
		flusher.Flush()
		<-r.Context().Done()
		close(backendCanceled)
	}))
	defer backend.Close()

	backendURL, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatalf("parse backend URL: %v", err)
	}
	localHost, localPortText, err := net.SplitHostPort(backendURL.Host)
	if err != nil {
		t.Fatalf("split backend host: %v", err)
	}
	localPort, err := strconv.Atoi(localPortText)
	if err != nil {
		t.Fatalf("parse backend port: %v", err)
	}

	poolClient := &PoolClient{
		tunnelType: protocol.TunnelTypeHTTP,
		localHost:  localHost,
		localPort:  localPort,
		httpClient: newLocalHTTPClient(protocol.TunnelTypeHTTP, false),
		ctx:        context.Background(),
		stats:      stats.NewTrafficStats(),
		logger:     zap.NewNop(),
	}

	serverSide, rawClientSide := net.Pipe()
	clientSide := &recordingConn{Conn: rawClientSide}

	done := make(chan struct{})
	go func() {
		defer close(done)
		poolClient.handleHTTPStream(clientSide)
	}()

	req, err := http.NewRequest(http.MethodGet, "http://demo.tunnels.test/events", nil)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	if err := req.Write(serverSide); err != nil {
		t.Fatalf("write request to stream: %v", err)
	}

	resp, err := http.ReadResponse(bufio.NewReader(serverSide), req)
	if err != nil {
		t.Fatalf("read response from stream: %v", err)
	}
	defer resp.Body.Close()

	got := readClientBodyWithin(t, resp.Body, len("data: first\n\n"), time.Second)
	if got != "data: first\n\n" {
		t.Fatalf("body prefix = %q, want first SSE event", got)
	}

	_ = serverSide.Close()
	select {
	case <-backendCanceled:
	case <-time.After(time.Second):
		t.Fatalf("backend request was not canceled after tunnel close")
	}

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("handleHTTPStream did not return after tunnel close")
	}
}

func readClientBodyWithin(t *testing.T, body io.Reader, size int, timeout time.Duration) string {
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
