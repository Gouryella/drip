package tcp

import (
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/quic-go/quic-go/http3"
	"go.uber.org/zap"
)

// startHTTP3TestListener starts a listener on an ephemeral TCP port with a
// self-signed certificate. HTTP/3 binds the matching UDP port; when the
// environment does not permit that, the caller skips.
func startHTTP3TestListener(t *testing.T, handler http.Handler, http3Enabled bool) *Listener {
	t.Helper()

	seed := httptest.NewTLSServer(http.NotFoundHandler())
	tlsConfig := seed.TLS.Clone()
	seed.Close()
	tlsConfig.MinVersion = tls.VersionTLS13

	l := NewListener(ListenerConfig{
		Address:      "127.0.0.1:0",
		TLSConfig:    tlsConfig,
		Logger:       zap.NewNop(),
		HTTPHandler:  handler,
		HTTP3Enabled: http3Enabled,
	})
	if err := l.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { _ = l.Stop() })

	if http3Enabled && l.h3Server == nil {
		t.Skip("HTTP/3 UDP listener could not be bound in this environment")
	}
	return l
}

func newHTTP3Client(t *testing.T) *http3.Transport {
	t.Helper()
	tr := &http3.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true, // self-signed test certificate
			MinVersion:         tls.VersionTLS13,
		},
	}
	t.Cleanup(func() { _ = tr.Close() })
	return tr
}

func http3EchoHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "proto=%d tls=%t remote=%s", r.ProtoMajor, r.TLS != nil, r.RemoteAddr)
	})
}

func TestHTTP3ServesSharedHandler(t *testing.T) {
	l := startHTTP3TestListener(t, http3EchoHandler(), true)
	client := &http.Client{Transport: newHTTP3Client(t)}

	resp, err := client.Get("https://" + l.h3Server.conn.LocalAddr().String() + "/")
	if err != nil {
		t.Fatalf("HTTP/3 request failed: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading response: %v", err)
	}
	if resp.ProtoMajor != 3 {
		t.Errorf("response protocol = HTTP/%d, want HTTP/3", resp.ProtoMajor)
	}
	if got := resp.Header.Get("Alt-Svc"); got != "" {
		t.Errorf("Alt-Svc on HTTP/3 response = %q, want none", got)
	}
	if got := string(body); !strings.HasPrefix(got, "proto=3 tls=true remote=127.0.0.1:") {
		t.Errorf("handler saw %q, want HTTP/3 with TLS state and remote address", got)
	}
}

func TestAltSvcAdvertisedOnHTTP1AndHTTP2(t *testing.T) {
	l := startHTTP3TestListener(t, http3EchoHandler(), true)
	wantAltSvc := fmt.Sprintf(`h3=":%d"; ma=86400`, l.h3Server.advertised)

	h1Transport := &http.Transport{TLSClientConfig: &tls.Config{
		InsecureSkipVerify: true, // self-signed test certificate
		MinVersion:         tls.VersionTLS13,
	}}
	t.Cleanup(h1Transport.CloseIdleConnections)

	h2Transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true, // self-signed test certificate
			MinVersion:         tls.VersionTLS13,
		},
		ForceAttemptHTTP2: true,
	}
	t.Cleanup(h2Transport.CloseIdleConnections)

	tests := []struct {
		name      string
		transport *http.Transport
		wantProto int
	}{
		{name: "http/1.1", transport: h1Transport, wantProto: 1},
		{name: "http/2", transport: h2Transport, wantProto: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &http.Client{Transport: tt.transport}
			resp, err := client.Get("https://" + l.listener.Addr().String() + "/")
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
			defer resp.Body.Close()
			_, _ = io.Copy(io.Discard, resp.Body)

			if resp.ProtoMajor != tt.wantProto {
				t.Errorf("response protocol = HTTP/%d, want HTTP/%d", resp.ProtoMajor, tt.wantProto)
			}
			if got := resp.Header.Get("Alt-Svc"); got != wantAltSvc {
				t.Errorf("Alt-Svc = %q, want %q", got, wantAltSvc)
			}
		})
	}
}

func TestAltSvcAbsentWhenHTTP3Disabled(t *testing.T) {
	l := startHTTP3TestListener(t, http3EchoHandler(), false)
	if l.h3Server != nil {
		t.Fatal("HTTP/3 server started while disabled")
	}

	transport := &http.Transport{TLSClientConfig: &tls.Config{
		InsecureSkipVerify: true, // self-signed test certificate
		MinVersion:         tls.VersionTLS13,
	}}
	t.Cleanup(transport.CloseIdleConnections)

	resp, err := (&http.Client{Transport: transport}).Get("https://" + l.listener.Addr().String() + "/")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if got := resp.Header.Get("Alt-Svc"); got != "" {
		t.Errorf("Alt-Svc = %q when HTTP/3 is disabled, want none", got)
	}
}

// HTTP/3 forbids connection-specific headers, so quic-go strips them from
// client requests and rejects them from raw peers. The wrapper is still tested
// directly as defense in depth for any future extended-CONNECT path.
func TestHTTP3HandlerRejectsWebSocketUpgrade(t *testing.T) {
	called := false
	h := newHTTP3Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	upgrade := httptest.NewRequest(http.MethodGet, "https://tunnel.example.test/ws", nil)
	upgrade.Header.Set("Connection", "Upgrade")
	upgrade.Header.Set("Upgrade", "websocket")
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, upgrade)

	if recorder.Code != http.StatusNotImplemented {
		t.Errorf("upgrade status = %d, want %d", recorder.Code, http.StatusNotImplemented)
	}
	if called {
		t.Error("next handler was called for a WebSocket upgrade")
	}

	plain := httptest.NewRequest(http.MethodGet, "https://tunnel.example.test/", nil)
	recorder = httptest.NewRecorder()
	h.ServeHTTP(recorder, plain)

	if recorder.Code != http.StatusOK {
		t.Errorf("plain request status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if !called {
		t.Error("next handler was not called for a plain request")
	}
}

func TestHTTP3StopDoesNotHang(t *testing.T) {
	l := startHTTP3TestListener(t, http.NotFoundHandler(), true)

	done := make(chan struct{})
	go func() {
		_ = l.Stop()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Listener.Stop() hung with HTTP/3 enabled")
	}
}
