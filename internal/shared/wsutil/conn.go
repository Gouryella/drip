package wsutil

import (
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Conn wraps a gorilla/websocket.Conn to implement net.Conn.
// It uses binary messages for data transfer, making it compatible
// with yamux and the existing frame protocol.
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
	writeDeadline time.Time
	writeTimer    *time.Timer
	writeActive   bool
	closed        bool
	writeTimedOut bool
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
	c.deadlineMu.Lock()
	if c.closed {
		c.deadlineMu.Unlock()
		return 0, net.ErrClosed
	}
	if !c.writeDeadline.IsZero() && !time.Now().Before(c.writeDeadline) {
		c.deadlineMu.Unlock()
		return 0, os.ErrDeadlineExceeded
	}
	// Keep gorilla's socket deadline unset. Our timer handles deadline
	// updates without racing the deadline gorilla applies when flushing.
	_ = c.ws.SetWriteDeadline(time.Time{})
	c.writeActive = true
	c.resetWriteTimerLocked()
	c.deadlineMu.Unlock()
	defer func() {
		c.deadlineMu.Lock()
		c.writeActive = false
		c.resetWriteTimerLocked()
		c.deadlineMu.Unlock()
	}()

	err := c.ws.WriteMessage(websocket.BinaryMessage, p)
	if err != nil {
		c.deadlineMu.Lock()
		timedOut := c.writeTimedOut
		c.deadlineMu.Unlock()
		if timedOut {
			return 0, os.ErrDeadlineExceeded
		}
		return 0, err
	}
	return len(p), nil
}

// Close closes the WebSocket connection.
func (c *Conn) Close() error {
	c.closeOnce.Do(func() {
		c.pingOnce.Do(func() { close(c.pingStop) })
		c.deadlineMu.Lock()
		c.closed = true
		c.writeActive = false
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
	c.deadlineMu.Lock()
	defer c.deadlineMu.Unlock()
	if c.closed {
		return net.ErrClosed
	}
	c.writeDeadline = t
	c.resetWriteTimerLocked()
	// Do not wait for writeMu: deadline changes must interrupt an ongoing
	// Write. Only Write touches gorilla's non-concurrent deadline field.
	return nil
}

func (c *Conn) resetWriteTimerLocked() {
	if c.writeTimer != nil {
		c.writeTimer.Stop()
		c.writeTimer = nil
	}
	if !c.writeActive || c.writeDeadline.IsZero() {
		return
	}
	deadline := c.writeDeadline
	c.writeTimer = time.AfterFunc(time.Until(deadline), func() {
		c.deadlineMu.Lock()
		expired := c.writeActive && c.writeDeadline.Equal(deadline) && !time.Now().Before(deadline)
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
