package tcp

import (
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// connQueueListener is a net.Listener backed by a channel of pre-accepted conns.
// It lets the TCP/TLS multiplexer hand off HTTP connections to a standard http.Server.
type connQueueListener struct {
	addr   net.Addr
	conns  chan net.Conn
	done   chan struct{}
	once   sync.Once
	closed atomic.Bool
}

func newConnQueueListener(addr net.Addr, buffer int) *connQueueListener {
	if buffer <= 0 {
		buffer = 1024
	}
	return &connQueueListener{
		addr:  addr,
		conns: make(chan net.Conn, buffer),
		done:  make(chan struct{}),
	}
}

func (l *connQueueListener) Accept() (net.Conn, error) {
	select {
	case <-l.done:
		return nil, net.ErrClosed
	case conn := <-l.conns:
		if conn == nil {
			return nil, net.ErrClosed
		}
		return conn, nil
	}
}

func (l *connQueueListener) Close() error {
	l.once.Do(func() {
		l.closed.Store(true)
		close(l.done)
		l.drain()
	})
	return nil
}

func (l *connQueueListener) Addr() net.Addr { return l.addr }

func (l *connQueueListener) Enqueue(conn net.Conn) bool {
	if conn == nil {
		return false
	}
	if l.closed.Load() {
		return false
	}

	select {
	case l.conns <- conn:
		return true
	default:
		return false
	}
}

func (l *connQueueListener) drain() {
	for {
		select {
		case conn := <-l.conns:
			if conn == nil {
				continue
			}
			_ = conn.SetDeadline(time.Now())
			_ = conn.Close()
		default:
			return
		}
	}
}
