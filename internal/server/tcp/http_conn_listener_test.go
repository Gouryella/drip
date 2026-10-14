package tcp

import (
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type closeTrackingConn struct {
	net.Conn
	closeCount    atomic.Int64
	deadlineCount atomic.Int64
}

func (c *closeTrackingConn) Close() error {
	c.closeCount.Add(1)
	return c.Conn.Close()
}

func (c *closeTrackingConn) SetDeadline(t time.Time) error {
	c.deadlineCount.Add(1)
	return c.Conn.SetDeadline(t)
}

func TestConnQueueListenerEnqueueAfterCloseReturnsFalseWithoutClosing(t *testing.T) {
	t.Parallel()

	listener := newConnQueueListener(dummyAddr("listener"), 1)
	if err := listener.Close(); err != nil {
		t.Fatalf("Close() failed: %v", err)
	}

	server, client := net.Pipe()
	defer client.Close()

	conn := &closeTrackingConn{Conn: server}
	if listener.Enqueue(conn) {
		t.Fatal("Enqueue returned true after Close")
	}
	if got := conn.closeCount.Load(); got != 0 {
		t.Fatalf("Enqueue closed rejected conn %d times, want 0", got)
	}

	_ = conn.Close()
}

func TestConnQueueListenerCloseDrainsQueuedConnections(t *testing.T) {
	t.Parallel()

	listener := newConnQueueListener(dummyAddr("listener"), 64)
	conns := make([]*closeTrackingConn, 0, 16)

	for i := 0; i < 16; i++ {
		server, client := net.Pipe()
		defer client.Close()

		conn := &closeTrackingConn{Conn: server}
		if !listener.Enqueue(conn) {
			t.Fatal("Enqueue returned false before Close")
		}
		conns = append(conns, conn)
	}

	if err := listener.Close(); err != nil {
		t.Fatalf("Close() failed: %v", err)
	}

	for i, conn := range conns {
		if got := conn.closeCount.Load(); got != 1 {
			t.Fatalf("queued conn %d close count = %d, want 1", i, got)
		}
	}
}

func TestConnQueueListenerConcurrentEnqueueCloseNoQueuedLeak(t *testing.T) {
	t.Parallel()

	for iteration := 0; iteration < 50; iteration++ {
		listener := newConnQueueListener(dummyAddr("listener"), 2048)
		start := make(chan struct{})
		var wg sync.WaitGroup
		var accepted sync.Map

		for i := 0; i < 128; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				server, client := net.Pipe()
				_ = client.Close()

				conn := &closeTrackingConn{Conn: server}
				<-start
				if listener.Enqueue(conn) {
					accepted.Store(conn, struct{}{})
					return
				}
				_ = conn.Close()
			}()
		}

		close(start)
		if err := listener.Close(); err != nil {
			t.Fatalf("Close() failed: %v", err)
		}
		wg.Wait()

		accepted.Range(func(key, _ interface{}) bool {
			conn := key.(*closeTrackingConn)
			if got := conn.closeCount.Load(); got != 1 {
				t.Fatalf("accepted conn close count = %d, want 1", got)
			}
			return true
		})
	}
}

type dummyAddr string

func (a dummyAddr) Network() string { return "test" }
func (a dummyAddr) String() string  { return string(a) }
