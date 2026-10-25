package cli

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"drip/internal/client/tcp"
	"drip/internal/shared/protocol"
	"drip/internal/shared/stats"
	"drip/pkg/config"

	"go.uber.org/zap"
)

type fakeTunnelClient struct {
	connectFunc func() error
	closeFunc   func() error
	waitFunc    func()
	url         string
	subdomain   string
}

func (f *fakeTunnelClient) Connect() error {
	if f.connectFunc != nil {
		return f.connectFunc()
	}
	return nil
}

func (f *fakeTunnelClient) Close() error {
	if f.closeFunc != nil {
		return f.closeFunc()
	}
	return nil
}

func (f *fakeTunnelClient) Wait() {
	if f.waitFunc != nil {
		f.waitFunc()
	}
}

func (f *fakeTunnelClient) GetURL() string {
	if f.url != "" {
		return f.url
	}
	return "https://example.test"
}

func (f *fakeTunnelClient) GetSubdomain() string { return f.subdomain }
func (f *fakeTunnelClient) SetLatencyCallback(tcp.LatencyCallback) {
}
func (f *fakeTunnelClient) GetLatency() time.Duration     { return 0 }
func (f *fakeTunnelClient) GetStats() *stats.TrafficStats { return nil }
func (f *fakeTunnelClient) IsClosed() bool                { return false }

func TestStartMultipleTunnelsCancelsStartedTunnelOnFailure(t *testing.T) {
	oldNewTunnelClient := newTunnelClient
	newTunnelClient = func(cfg *tcp.ConnectorConfig, _ *zap.Logger) tcp.TunnelClient {
		t.Fatalf("unexpected tunnel client for port %d before test setup", cfg.LocalPort)
		return nil
	}
	t.Cleanup(func() {
		newTunnelClient = oldNewTunnelClient
	})

	started := make(chan struct{})
	closed := make(chan struct{})
	var startedOnce sync.Once
	var closedOnce sync.Once

	newTunnelClient = func(cfg *tcp.ConnectorConfig, _ *zap.Logger) tcp.TunnelClient {
		switch cfg.LocalPort {
		case 3000:
			return &fakeTunnelClient{
				url: "https://ok.example.test",
				connectFunc: func() error {
					startedOnce.Do(func() { close(started) })
					return nil
				},
				closeFunc: func() error {
					closedOnce.Do(func() { close(closed) })
					return nil
				},
			}
		case 3001:
			return &fakeTunnelClient{
				connectFunc: func() error {
					select {
					case <-started:
					case <-time.After(2 * time.Second):
						return errors.New("successful tunnel did not start")
					}
					return errors.New("dial failed")
				},
			}
		default:
			return &fakeTunnelClient{}
		}
	}

	cfg := &config.ClientConfig{
		Server: "drip.example:443",
		Token:  "token",
	}
	tunnels := []*config.TunnelConfig{
		{Name: "ok", Type: "http", Port: 3000},
		{Name: "bad", Type: "http", Port: 3001},
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- startMultipleTunnels(cfg, tunnels)
	}()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatalf("startMultipleTunnels() error = nil, want failure")
		}
		if !strings.Contains(err.Error(), "1 tunnel(s) failed to start") {
			t.Fatalf("startMultipleTunnels() error = %v, want failed tunnel count", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("startMultipleTunnels() hung after one tunnel failed")
	}

	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatalf("started tunnel was not closed after another tunnel failed")
	}
}

func TestRunTunnelWithUIUsesBackoffAndReturnsLastErr(t *testing.T) {
	oldNewTunnelClient := newTunnelClient
	oldNextReconnectWait := nextReconnectWait
	oldReconnectTimer := reconnectTimer
	oldRunnerNow := tunnelRunnerNow
	t.Cleanup(func() {
		newTunnelClient = oldNewTunnelClient
		nextReconnectWait = oldNextReconnectWait
		reconnectTimer = oldReconnectTimer
		tunnelRunnerNow = oldRunnerNow
	})

	var attempts int
	newTunnelClient = func(_ *tcp.ConnectorConfig, _ *zap.Logger) tcp.TunnelClient {
		attempts++
		err := fmt.Errorf("dial failure %d", attempts)
		return &fakeTunnelClient{connectFunc: func() error { return err }}
	}

	var waits []time.Duration
	nextReconnectWait = func(attempt int) time.Duration {
		wait := time.Duration(1<<(attempt-1)) * time.Second
		waits = append(waits, wait)
		return wait
	}
	reconnectTimer = func(time.Duration) <-chan time.Time {
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		return ch
	}

	err := runTunnelWithUI(&tcp.ConnectorConfig{
		ServerAddr: "drip.example:443",
		Token:      "token",
		TunnelType: protocol.TunnelTypeHTTP,
		LocalHost:  "127.0.0.1",
		LocalPort:  3000,
	}, nil)
	if err == nil {
		t.Fatalf("runTunnelWithUI() error = nil, want retry exhaustion")
	}
	if !strings.Contains(err.Error(), "dial failure 5") {
		t.Fatalf("runTunnelWithUI() error = %v, want last connect error", err)
	}
	if attempts != maxReconnectAttempts {
		t.Fatalf("connect attempts = %d, want %d", attempts, maxReconnectAttempts)
	}

	wantWaits := []time.Duration{
		1 * time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
	}
	if !reflect.DeepEqual(waits, wantWaits) {
		t.Fatalf("reconnect waits = %v, want %v", waits, wantWaits)
	}
}

func TestDefaultReconnectWaitIsCappedExponentialWithJitter(t *testing.T) {
	for attempt := 1; attempt <= 10; attempt++ {
		wait := defaultReconnectWait(attempt)
		base := reconnectBaseInterval
		for i := 1; i < attempt; i++ {
			base *= 2
			if base > reconnectMaxInterval {
				base = reconnectMaxInterval
				break
			}
		}

		if wait < base/2 || wait > base {
			t.Fatalf("defaultReconnectWait(%d) = %v, want between %v and %v", attempt, wait, base/2, base)
		}
		if wait > reconnectMaxInterval {
			t.Fatalf("defaultReconnectWait(%d) = %v, exceeds cap %v", attempt, wait, reconnectMaxInterval)
		}
	}
}
