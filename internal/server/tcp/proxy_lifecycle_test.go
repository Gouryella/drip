package tcp

import (
	"context"
	"net"
	"sync"
	"testing"

	"go.uber.org/zap"
)

// noopStats satisfies trafficStats without recording anything.
type noopStats struct{}

func (noopStats) AddBytesIn(int64)      {}
func (noopStats) AddBytesOut(int64)     {}
func (noopStats) IncActiveConnections() {}
func (noopStats) DecActiveConnections() {}

func newTestProxy(t *testing.T) (*Proxy, net.Listener) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}

	openStream := func() (net.Conn, error) { return nil, net.ErrClosed }
	p := NewProxy(context.Background(), 0, "test", openStream, noopStats{}, zap.NewNop())
	return p, ln
}

// Stop may run from the connection's shutdown path while the connection
// goroutine is still starting the proxy. Racing wg.Add against wg.Wait is a
// data race, and worse, a proxy stopped mid-startup used to leave its accept
// loop running after Stop had returned.
func TestProxyStopRacingStart(t *testing.T) {
	for i := 0; i < 100; i++ {
		p, ln := newTestProxy(t)

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			// Either outcome is valid; only a race or a leak is not.
			_ = p.StartWithListener(ln)
		}()
		go func() {
			defer wg.Done()
			p.Stop()
		}()
		wg.Wait()

		// Stop must have fully drained: a second Stop returns immediately, and
		// the listener is closed either way.
		p.Stop()
		_ = ln.Close()
	}
}

// Starting a proxy that has already stopped must fail rather than spawn an
// accept loop nothing will ever wait for.
func TestProxyStartAfterStopIsRefused(t *testing.T) {
	p, ln := newTestProxy(t)
	t.Cleanup(func() { _ = ln.Close() })

	p.Stop()

	if err := p.StartWithListener(ln); err == nil {
		t.Fatal("StartWithListener() after Stop = nil error, want failure")
	}
}

// The normal path still works, and Stop is idempotent.
func TestProxyStartThenStop(t *testing.T) {
	p, ln := newTestProxy(t)

	if err := p.StartWithListener(ln); err != nil {
		t.Fatalf("StartWithListener() error = %v", err)
	}

	p.Stop()
	p.Stop()
}
