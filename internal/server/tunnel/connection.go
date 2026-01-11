package tunnel

import (
	"net"
	"sync"
	"sync/atomic"
	"time"

	"drip/internal/server/metrics"
	"drip/internal/shared/netutil"
	"drip/internal/shared/protocol"
	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

// Connection represents a tunnel connection from a client
type Connection struct {
	Subdomain  string
	Conn       *websocket.Conn
	SendCh     chan []byte
	CloseCh    chan struct{}
	LastActive time.Time
	mu         sync.RWMutex
	logger     *zap.Logger
	closed     atomic.Bool // Use atomic for lock-free hot path checks
	tunnelType protocol.TunnelType
	openStream func() (net.Conn, error)
	remoteIP   string // Client IP for rate limiting tracking

	bytesIn           atomic.Int64
	bytesOut          atomic.Int64
	activeConnections atomic.Int64

	ipAccessChecker *netutil.IPAccessChecker
}

// NewConnection creates a new tunnel connection
func NewConnection(subdomain string, conn *websocket.Conn, logger *zap.Logger) *Connection {
	return &Connection{
		Subdomain:  subdomain,
		Conn:       conn,
		SendCh:     make(chan []byte, 256),
		CloseCh:    make(chan struct{}),
		LastActive: time.Now(),
		logger:     logger,
	}
}

// Send sends data through the WebSocket connection
func (c *Connection) Send(data []byte) error {
	// Lock-free check using atomic - avoids RLock contention on hot path
	if c.closed.Load() {
		return ErrConnectionClosed
	}

	select {
	case c.SendCh <- data:
		return nil
	case <-time.After(5 * time.Second):
		return ErrSendTimeout
	}
}

// UpdateActivity updates the last activity timestamp
func (c *Connection) UpdateActivity() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.LastActive = time.Now()
}

// IsAlive checks if the connection is still alive based on last activity
func (c *Connection) IsAlive(timeout time.Duration) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return time.Since(c.LastActive) < timeout
}

// Close closes the connection and all associated channels
func (c *Connection) Close() {
	// Use atomic swap to ensure only one goroutine closes
	if c.closed.Swap(true) {
		return // Already closed
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	close(c.CloseCh)
	close(c.SendCh)

	if c.Conn != nil {
		// Send close message
		c.Conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		c.Conn.Close()
	}

	c.logger.Info("Connection closed",
		zap.String("subdomain", c.Subdomain),
	)
}

// IsClosed returns whether the connection is closed
func (c *Connection) IsClosed() bool {
	return c.closed.Load() // Lock-free check
}

// SetTunnelType sets the tunnel type.
func (c *Connection) SetTunnelType(tType protocol.TunnelType) {
	c.mu.Lock()
	c.tunnelType = tType
	c.mu.Unlock()
}

// GetTunnelType returns the tunnel type.
func (c *Connection) GetTunnelType() protocol.TunnelType {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.tunnelType
}

// SetOpenStream registers a stream opener for this tunnel.
func (c *Connection) SetOpenStream(open func() (net.Conn, error)) {
	c.mu.Lock()
	c.openStream = open
	c.mu.Unlock()
}

// OpenStream opens a new mux stream to the tunnel client.
func (c *Connection) OpenStream() (net.Conn, error) {
	// Lock-free closed check
	if c.closed.Load() {
		return nil, ErrConnectionClosed
	}

	c.mu.RLock()
	open := c.openStream
	c.mu.RUnlock()

	if open == nil {
		return nil, ErrConnectionClosed
	}
	return open()
}

func (c *Connection) AddBytesIn(n int64) {
	if n <= 0 {
		return
	}
	c.bytesIn.Add(n)
	metrics.BytesReceived.Add(float64(n))
	metrics.TunnelBytesReceived.WithLabelValues(c.Subdomain, c.Subdomain, c.GetTunnelType().String()).Add(float64(n))
}

func (c *Connection) AddBytesOut(n int64) {
	if n <= 0 {
		return
	}
	c.bytesOut.Add(n)
	metrics.BytesSent.Add(float64(n))
	metrics.TunnelBytesSent.WithLabelValues(c.Subdomain, c.Subdomain, c.GetTunnelType().String()).Add(float64(n))
}

func (c *Connection) GetBytesIn() int64 {
	return c.bytesIn.Load()
}

func (c *Connection) GetBytesOut() int64 {
	return c.bytesOut.Load()
}

func (c *Connection) IncActiveConnections() {
	c.activeConnections.Add(1)
	metrics.TunnelActiveConnections.WithLabelValues(c.Subdomain, c.Subdomain, c.GetTunnelType().String()).Inc()
}

func (c *Connection) DecActiveConnections() {
	if v := c.activeConnections.Add(-1); v < 0 {
		c.activeConnections.Store(0)
	}
	metrics.TunnelActiveConnections.WithLabelValues(c.Subdomain, c.Subdomain, c.GetTunnelType().String()).Dec()
}

func (c *Connection) GetActiveConnections() int64 {
	return c.activeConnections.Load()
}

// SetIPAccessControl sets the IP access control rules for this tunnel.
func (c *Connection) SetIPAccessControl(allowCIDRs, denyIPs []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ipAccessChecker = netutil.NewIPAccessChecker(allowCIDRs, denyIPs)
}

// IsIPAllowed checks if the given IP address is allowed to access this tunnel.
func (c *Connection) IsIPAllowed(ip string) bool {
	c.mu.RLock()
	checker := c.ipAccessChecker
	c.mu.RUnlock()

	if checker == nil {
		return true // No access control configured
	}
	return checker.IsAllowed(ip)
}

// HasIPAccessControl returns true if IP access control is configured.
func (c *Connection) HasIPAccessControl() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.ipAccessChecker != nil && c.ipAccessChecker.HasRules()
}

// StartWritePump starts the write pump for sending messages
func (c *Connection) StartWritePump() {
	if c.Conn == nil {
		go func() {
			for {
				select {
				case <-c.SendCh:
				case <-c.CloseCh:
					return
				}
			}
		}()
		return
	}

	ticker := time.NewTicker(30 * time.Second)
	defer func() {
		ticker.Stop()
		c.Close()
	}()

	for {
		select {
		case message, ok := <-c.SendCh:
			if !ok {
				return
			}

			c.Conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.Conn.WriteMessage(websocket.TextMessage, message); err != nil {
				c.logger.Error("Write error",
					zap.String("subdomain", c.Subdomain),
					zap.Error(err),
				)
				return
			}

		case <-ticker.C:
			// Send ping to keep connection alive
			c.Conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.Conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}

		case <-c.CloseCh:
			return
		}
	}
}
