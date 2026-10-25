package tcp

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	clienttcp "drip/internal/client/tcp"
	"drip/internal/server/proxy"
	"drip/internal/server/tunnel"
	"drip/internal/shared/protocol"
	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

func integrationListener(t *testing.T, transport string) *Listener {
	t.Helper()
	seed := httptest.NewTLSServer(http.NotFoundHandler())
	tlsConfig := seed.TLS.Clone()
	seed.Close()
	tlsConfig.MinVersion = tls.VersionTLS13
	logger := zap.NewNop()
	manager := tunnel.NewManager(logger)
	t.Cleanup(manager.Shutdown)
	h := proxy.NewHandler(proxy.HandlerConfig{Manager: manager, Logger: logger, ServerDomain: "connect.example.test", TunnelDomain: "example.test"})
	h.SetAllowedTransports([]string{transport})
	ports, err := NewPortAllocator(20000, 40000)
	if err != nil {
		t.Fatal(err)
	}
	l := NewListener(ListenerConfig{
		Address: "127.0.0.1:0", TLSConfig: tlsConfig, AuthToken: "integration-test-token",
		Manager: manager, Logger: logger, PortAlloc: ports,
		Domain: "connect.example.test", TunnelDomain: "example.test", PublicPort: 443, HTTPHandler: h,
	})
	l.SetAllowedTransports([]string{transport})
	if err := l.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Stop() })
	return l
}

func connectIntegrationClient(t *testing.T, l *Listener, backend, transport string, kind protocol.TunnelType, subdomain string) *clienttcp.PoolClient {
	t.Helper()
	host, portText, err := net.SplitHostPort(backend)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(portText)
	c := clienttcp.NewPoolClient(&clienttcp.ConnectorConfig{
		Token:      "integration-test-token",
		ServerAddr: l.listener.Addr().String(), Insecure: true, Transport: clienttcp.TransportType(transport),
		TunnelType: kind, LocalHost: host, LocalPort: port, Subdomain: subdomain,
		PoolMin: 1, PoolSize: 2, PoolMax: 2,
	}, zap.NewNop())
	t.Cleanup(func() {
		_ = c.Close()
		done := make(chan struct{})
		go func() { c.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("tunnel client did not shut down")
		}
	})
	if err := c.Connect(); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestEndToEndHTTPAndWebSocket(t *testing.T) {
	for _, transport := range []string{"tcp", "wss"} {
		t.Run(transport, func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/ws" {
					conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
					if err != nil {
						return
					}
					defer conn.Close()
					for {
						kind, payload, err := conn.ReadMessage()
						if err != nil {
							return
						}
						if err := conn.WriteMessage(kind, payload); err != nil {
							return
						}
					}
				}
				if r.Header.Get("X-Forwarded-Proto") != "https" || r.Header.Get("X-Real-IP") != "127.0.0.1" {
					t.Errorf("forwarded metadata = %v", r.Header)
				}
				w.Header().Set("Content-Type", "application/octet-stream")
				if r.Header.Get("Accept-Encoding") != "gzip" {
					t.Error("compression negotiation was stripped")
				}
				if r.URL.Path == "/trailers" {
					w.Header().Set("Trailer", "X-Response-Checksum")
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				_, _ = w.Write(body)
				if r.URL.Path == "/trailers" {
					if r.Trailer.Get("X-Request-Checksum") != "checked" {
						t.Error("request trailers were lost")
					}
					w.Header().Set("X-Response-Checksum", "verified")
				}
			}))
			t.Cleanup(backend.Close)
			l := integrationListener(t, transport)
			if transport == "wss" {
				dialer := websocket.Dialer{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, HandshakeTimeout: time.Second}
				ws, resp, err := dialer.Dial("wss://"+l.listener.Addr().String()+"/_drip/ws", nil)
				if ws != nil {
					_ = ws.Close()
				}
				if resp != nil {
					_ = resp.Body.Close()
				}
				if err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
					t.Fatalf("unauthenticated tunnel upgrade: response=%v error=%v", resp, err)
				}
			}
			connectIntegrationClient(t, l, backend.Listener.Addr().String(), transport, protocol.TunnelTypeHTTP, "e2e-http")
			publicClient := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, ForceAttemptHTTP2: true}}
			defer publicClient.CloseIdleConnections()
			payload := bytes.Repeat([]byte("drip-data-"), 4096)
			var wg sync.WaitGroup
			for i := 0; i < 12; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					req, _ := http.NewRequest(http.MethodPost, "https://"+l.listener.Addr().String()+"/echo", bytes.NewReader(payload))
					req.Host = "e2e-http.example.test"
					req.Header.Set("Accept-Encoding", "gzip")
					if i == 0 {
						req.URL.Path = "/trailers"
						req.Trailer = http.Header{"X-Request-Checksum": {"checked"}}
					}
					req.Header.Set("X-Real-IP", "10.0.0.1")
					resp, err := publicClient.Do(req)
					if err != nil {
						t.Error(err)
						return
					}
					defer resp.Body.Close()
					got, err := io.ReadAll(resp.Body)
					if err != nil || !bytes.Equal(got, payload) || resp.ProtoMajor != 2 {
						t.Errorf("echo: %s, %d bytes, %v", resp.Proto, len(got), err)
					}
					if i == 0 && resp.Trailer.Get("X-Response-Checksum") != "verified" {
						t.Error("response trailers were lost")
					}
				}()
			}
			wg.Wait()
			dialer := websocket.Dialer{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, HandshakeTimeout: 5 * time.Second}
			ws, _, err := dialer.Dial("wss://"+l.listener.Addr().String()+"/ws", http.Header{"Host": {"e2e-http.example.test"}})
			if err != nil {
				t.Fatal(err)
			}
			defer ws.Close()
			_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
			for i := 0; i < 3; i++ {
				message := bytes.Repeat([]byte(fmt.Sprintf("ws-%d", i)), 4096)
				if err := ws.WriteMessage(websocket.BinaryMessage, message); err != nil {
					t.Fatal(err)
				}
				_, got, err := ws.ReadMessage()
				if err != nil || !bytes.Equal(got, message) {
					t.Fatalf("WebSocket echo: %d bytes, %v", len(got), err)
				}
			}
		})
	}
}

func TestEndToEndTCPHalfClose(t *testing.T) {
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	payload := bytes.Repeat([]byte("request"), 32768)
	backendDone := make(chan error, 1)
	go func() {
		conn, err := backend.Accept()
		if err != nil {
			backendDone <- err
			return
		}
		defer conn.Close()
		got, err := io.ReadAll(conn)
		if err == nil && !bytes.Equal(got, payload) {
			err = fmt.Errorf("backend request had %d bytes", len(got))
		}
		if err == nil {
			_, err = conn.Write(got)
		}
		backendDone <- err
	}()
	l := integrationListener(t, "tcp")
	c := connectIntegrationClient(t, l, backend.Addr().String(), "tcp", protocol.TunnelTypeTCP, "")
	tunnelURL, err := url.Parse(c.GetURL())
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", tunnelURL.Port()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(conn)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("TCP half-close response: %d bytes, %v", len(got), err)
	}
	if err := <-backendDone; err != nil {
		t.Fatal(err)
	}
}
