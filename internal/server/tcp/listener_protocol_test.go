package tcp

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"drip/internal/server/tunnel"
	"drip/internal/shared/pool"
	"drip/internal/shared/protocol"
	"go.uber.org/zap"
)

func TestListenerPreservesTLSStateWithoutALPN(t *testing.T) {
	seed := httptest.NewTLSServer(http.NotFoundHandler())
	tlsConfig := seed.TLS.Clone()
	seed.Close()
	tlsConfig.MinVersion = tls.VersionTLS13
	l := newProtocolTestListener(t, tlsConfig)
	l.httpHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil {
			w.WriteHeader(http.StatusInternalServerError)
		} else {
			w.WriteHeader(http.StatusNoContent)
		}
	})
	if err := l.Start(); err != nil {
		t.Fatal(err)
	}
	conn, err := tls.Dial("tcp", l.listener.Addr().String(), &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	_, _ = fmt.Fprint(conn, "GET / HTTP/1.1\r\nHost: example.test\r\nConnection: close\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatal("HTTP handler lost TLS connection state")
	}
}

func newProtocolTestListener(t *testing.T, tlsConfig *tls.Config) *Listener {
	t.Helper()
	logger := zap.NewNop()
	manager := tunnel.NewManager(logger)
	t.Cleanup(manager.Shutdown)
	l := NewListener(ListenerConfig{
		Address: "127.0.0.1:0", TLSConfig: tlsConfig, AllowAnonymous: true,
		Manager: manager, Logger: logger, Domain: "example.test", TunnelDomain: "example.test",
		HTTPHandler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}),
	})
	l.workerPool.Close()
	l.workerPool = pool.NewWorkerPool(1, 4)
	t.Cleanup(func() { _ = l.Stop() })
	return l
}

func sendHTTPRegistration(t *testing.T, conn net.Conn) {
	t.Helper()
	payload, err := protocol.MarshalJSON(protocol.RegisterRequest{
		TunnelType: protocol.TunnelTypeHTTP, LocalPort: 3000, CustomSubdomain: "protocol-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if err := protocol.WriteFrame(conn, protocol.NewFrame(protocol.FrameTypeRegister, payload)); err != nil {
		t.Fatal(err)
	}
	frame := readFrameWithDeadline(t, conn)
	defer frame.Release()
	if frame.Type != protocol.FrameTypeRegisterAck {
		t.Fatalf("registration response = %s, %s", frame.Type, frame.Payload)
	}
	_ = conn.SetDeadline(time.Time{})
}

func TestListenerLongLivedTunnelDoesNotOccupyHandshakeWorker(t *testing.T) {
	l := newProtocolTestListener(t, nil)
	if err := l.Start(); err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", l.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	sendHTTPRegistration(t, conn)
	client := &http.Client{Timeout: time.Second}
	defer client.CloseIdleConnections()
	resp, err := client.Get("http://" + l.listener.Addr().String() + "/health")
	if err != nil {
		t.Fatalf("HTTP request behind one live tunnel: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("HTTP status = %d", resp.StatusCode)
	}
}

func TestListenerAllowsWebSocketOnlyTransport(t *testing.T) {
	l := newProtocolTestListener(t, nil)
	l.SetAllowedTransports([]string{"wss"})
	server, client := net.Pipe()
	defer client.Close()
	done := make(chan struct{})
	go func() { l.HandleWSConnection(server, "127.0.0.1"); close(done) }()
	sendHTTPRegistration(t, client)
	_ = client.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("WebSocket connection did not finish")
	}
}

func TestListenerServesNegotiatedHTTP2(t *testing.T) {
	seed := httptest.NewTLSServer(http.NotFoundHandler())
	tlsConfig := seed.TLS.Clone()
	seed.Close()
	tlsConfig.MinVersion = tls.VersionTLS13
	tlsConfig.MaxVersion = tls.VersionTLS13
	tlsConfig.NextProtos = []string{"h2", "http/1.1"}
	l := newProtocolTestListener(t, tlsConfig)
	if err := l.Start(); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{
		Timeout: time.Second,
		Transport: &http.Transport{
			TLSClientConfig:   &tls.Config{InsecureSkipVerify: true},
			ForceAttemptHTTP2: true,
		},
	}
	defer client.CloseIdleConnections()
	resp, err := client.Get("https://" + l.listener.Addr().String() + "/health")
	if err != nil {
		t.Fatalf("HTTP/2 request: %v", err)
	}
	resp.Body.Close()
	if resp.ProtoMajor != 2 || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("response = %s, status %d", resp.Proto, resp.StatusCode)
	}
}
