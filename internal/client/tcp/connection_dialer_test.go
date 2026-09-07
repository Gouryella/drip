package tcp

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"go.uber.org/zap"
)

func TestDiscoveryIsSharedByConcurrentPoolDials(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = fmt.Fprint(w, `{"transports":["tcp","wss"],"preferred":"tcp"}`)
	}))
	defer server.Close()
	dialer := NewConnectionDialer(strings.TrimPrefix(server.URL, "https://"), &tls.Config{InsecureSkipVerify: true}, "", TransportAuto, zap.NewNop())
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			caps := dialer.discoverServerCapabilities(context.Background())
			if caps == nil || caps.Preferred != "tcp" {
				t.Error("missing server capabilities")
			}
		}()
	}
	wg.Wait()
	if got := requests.Load(); got != 1 {
		t.Fatalf("discovery requests = %d, want 1", got)
	}
}

func TestServerAddressNormalization(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"example.test", "example.test:443"},
		{"example.test:8443", "example.test:8443"},
		{"wss://example.test", "example.test:443"},
		{"wss://[::1]:8443", "[::1]:8443"},
		{"wss://[::1]", "[::1]:443"},
		{"::1", "[::1]:443"},
	} {
		if got := normalizeServerAddress(tc.input); got != tc.want {
			t.Errorf("normalize(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
	c := NewPoolClient(&ConnectorConfig{ServerAddr: "wss://[::1]:8443", LocalPort: 3000}, zap.NewNop())
	defer c.Close()
	if c.transport != TransportWebSocket || c.serverAddr != "[::1]:8443" {
		t.Fatalf("transport/address = %q/%q", c.transport, c.serverAddr)
	}
}
