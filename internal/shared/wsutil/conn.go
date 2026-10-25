package wsutil

import (
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// Conn wraps a gorilla/websocket.Conn to implement net.Conn.
// It uses binary messages for data transfer, making it compatible
// with the multiplexer and the existing frame protocol.
type Conn struct {
	ws            *websocket.Conn
	reader        io.Reader
	readMu        sync.Mutex
	writeMu       sync.Mutex
	localAddr     net.Addr
	remoteAddr    net.Addr
	pingStop      chan struct{}
	pingOnce      sync.Once
	closeOnce     sync.Once
	closeErr      error
	deadlineMu    sync.Mutex
	writeDeadline atomic.Pointer[time.Time] // nil means no write deadline
	writeTimer    *time.Timer               // guarded by deadlineMu
	writeActive   atomic.Bool
	closed        atomic.Bool
	writeTimedOut bool // guarded by deadlineMu
}

// NewConn wraps a WebSocket connection as a net.Conn.
func NewConn(ws *websocket.Conn) *Conn {
	c := &Conn{
		ws:         ws,
		localAddr:  ws.LocalAddr(),
		remoteAddr: ws.RemoteAddr(),
		pingStop:   make(chan struct{}),
	}
	return c
}

// NewConnWithPing wraps a WebSocket connection and starts a ping loop
// to keep the connection alive through CDN/proxies.
func NewConnWithPing(ws *websocket.Conn, pingInterval time.Duration) *Conn {
	c := NewConn(ws)
	c.startPingLoop(pingInterval)
	return c
}

// Read reads data from the WebSocket connection.
// It handles WebSocket message boundaries transparently, presenting
// a continuous byte stream to the caller.
func (c *Conn) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	c.readMu.Lock()
	defer c.readMu.Unlock()

	for {
		if c.reader == nil {
			messageType, reader, err := c.ws.NextReader()
			if err != nil {
				return 0, err
			}
			// Only accept binary messages for tunnel data
			if messageType != websocket.BinaryMessage {
				// Skip non-binary messages (text, ping/pong handled by gorilla)
				continue
			}
			c.reader = reader
		}

		n, err := c.reader.Read(p)
		if err == io.EOF {
			// Current message exhausted, get next message
			c.reader = nil
			if n > 0 {
				return n, nil
			}
			continue
		}
		return n, err
	}
}

// Write writes data to the WebSocket connection as a binary message.
func (c *Conn) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	if c.closed.Load() {
		return 0, net.ErrClosed
	}

	// Publish the in-flight write before sampling the deadline. Together with
	// SetWriteDeadline storing the deadline before checking writeActive, this
	// guarantees that a deadline configured concurrently with Write is either
	// observed below or interrupts the write through the timer.
	c.writeActive.Store(true)

	if c.writeDeadline.Load() == nil {
		// Fast path: no write deadline is configured. This wrapper never sets
		// gorilla's write deadline (it starts zero and stays zero), so there
		// is no per-write deadline bookkeeping to do. A deadline installed
		// while this write is in flight still arms the interrupt timer.
		defer func() {
			c.writeActive.Store(false)
			if c.writeDeadline.Load() != nil {
				// A deadline appeared mid-write; stop the timer it armed.
				c.deadlineMu.Lock()
				c.resetWriteTimerLocked()
				c.deadlineMu.Unlock()
			}
		}()

		err := c.ws.WriteMessage(websocket.BinaryMessage, p)
		if err != nil {
			return 0, c.writeError(err)
		}
		return len(p), nil
	}

	// Deadline path: serialize with SetWriteDeadline so the timer is armed for
	// exactly the deadline in effect.
	c.deadlineMu.Lock()
	if deadline := c.writeDeadline.Load(); deadline != nil && !time.Now().Before(*deadline) {
		c.writeActive.Store(false)
		c.deadlineMu.Unlock()
		return 0, os.ErrDeadlineExceeded
	}
	c.resetWriteTimerLocked()
	c.deadlineMu.Unlock()
	defer func() {
		c.deadlineMu.Lock()
		c.writeActive.Store(false)
		c.resetWriteTimerLocked()
		c.deadlineMu.Unlock()
	}()

	err := c.ws.WriteMessage(websocket.BinaryMessage, p)
	if err != nil {
		return 0, c.writeError(err)
	}
	return len(p), nil
}

// writeError maps a failed write to os.ErrDeadlineExceeded when the write
// deadline timer closed the connection.
func (c *Conn) writeError(err error) error {
	c.deadlineMu.Lock()
	timedOut := c.writeTimedOut
	c.deadlineMu.Unlock()
	if timedOut {
		return os.ErrDeadlineExceeded
	}
	return err
}

// Close closes the WebSocket connection.
func (c *Conn) Close() error {
	c.closeOnce.Do(func() {
		c.pingOnce.Do(func() { close(c.pingStop) })
		c.closed.Store(true)
		c.writeActive.Store(false)
		c.deadlineMu.Lock()
		c.resetWriteTimerLocked()
		c.deadlineMu.Unlock()
		// Close must interrupt an in-flight writer. Waiting for writeMu or
		// sending a close frame here can deadlock behind a stalled peer.
		c.closeErr = c.ws.Close()
	})
	return c.closeErr
}

// LocalAddr returns the local network address.
func (c *Conn) LocalAddr() net.Addr {
	return c.localAddr
}

// RemoteAddr returns the remote network address.
func (c *Conn) RemoteAddr() net.Addr {
	return c.remoteAddr
}

// SetDeadline sets the read and write deadlines.
func (c *Conn) SetDeadline(t time.Time) error {
	if err := c.ws.SetReadDeadline(t); err != nil {
		return err
	}
	return c.SetWriteDeadline(t)
}

// SetReadDeadline sets the read deadline.
func (c *Conn) SetReadDeadline(t time.Time) error {
	return c.ws.SetReadDeadline(t)
}

// SetWriteDeadline sets the write deadline.
func (c *Conn) SetWriteDeadline(t time.Time) error {
	if c.closed.Load() {
		return net.ErrClosed
	}
	if t.IsZero() {
		c.writeDeadline.Store(nil)
	} else {
		deadline := t
		c.writeDeadline.Store(&deadline)
	}
	// Do not wait for writeMu: deadline changes must interrupt an ongoing
	// Write. Only Write serializes gorilla's write path.
	c.deadlineMu.Lock()
	c.resetWriteTimerLocked()
	c.deadlineMu.Unlock()
	return nil
}

func (c *Conn) resetWriteTimerLocked() {
	if c.writeTimer != nil {
		c.writeTimer.Stop()
		c.writeTimer = nil
	}
	deadline := c.writeDeadline.Load()
	if !c.writeActive.Load() || deadline == nil {
		return
	}
	d := *deadline
	c.writeTimer = time.AfterFunc(time.Until(d), func() {
		c.deadlineMu.Lock()
		current := c.writeDeadline.Load()
		expired := c.writeActive.Load() && current != nil && current.Equal(d) && !time.Now().Before(d)
		if expired {
			c.writeTimedOut = true
		}
		c.deadlineMu.Unlock()
		if expired {
			// Gorilla cannot reuse a connection after a write timeout. Closing
			// also covers a write that was about to reset the socket deadline.
			_ = c.Close()
		}
	})
}

// startPingLoop starts a goroutine that sends periodic ping messages
// to keep the connection alive through CDN/proxies like Cloudflare.
func (c *Conn) startPingLoop(interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-c.pingStop:
				return
			case <-ticker.C:
				err := c.ws.WriteControl(
					websocket.PingMessage,
					[]byte{},
					time.Now().Add(10*time.Second),
				)
				if err != nil {
					_ = c.Close()
					return
				}
			}
		}
	}()
}

// UnderlyingConn returns the underlying WebSocket connection.
// Use with caution as direct access bypasses the mutex protection.
func (c *Conn) UnderlyingConn() *websocket.Conn {
	return c.ws
}
