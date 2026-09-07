package tcp

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
)

type failingAcceptListener struct {
	attempts  atomic.Int64
	started   chan struct{}
	startOnce sync.Once
}

func (l *failingAcceptListener) Accept() (net.Conn, error) {
	l.attempts.Add(1)
	l.startOnce.Do(func() { close(l.started) })
	return nil, errors.New("accept: resource exhausted")
}
func (l *failingAcceptListener) Close() error   { return nil }
func (l *failingAcceptListener) Addr() net.Addr { return &net.TCPAddr{} }

func TestAcceptErrorsBackOffAndRemainCancelable(t *testing.T) {
	for _, kind := range []string{"listener", "proxy"} {
		t.Run(kind, func(t *testing.T) {
			ln := &failingAcceptListener{started: make(chan struct{})}
			var stop func()
			if kind == "listener" {
				l := NewListener(ListenerConfig{Logger: zap.NewNop()})
				l.listener = ln
				l.wg.Add(1)
				go l.acceptLoop()
				stop = func() { _ = l.Stop() }
			} else {
				p := NewProxy(context.Background(), 0, "backoff", nil, nil, zap.NewNop())
				p.listener = ln
				p.wg.Add(1)
				go p.acceptLoop()
				stop = p.Stop
			}
			defer stop()
			<-ln.started
			time.Sleep(30 * time.Millisecond)
			stop()
			if got := ln.attempts.Load(); got > 10 {
				t.Fatalf("accept loop spun %d times instead of backing off", got)
			}
		})
	}
}
