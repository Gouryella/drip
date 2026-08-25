package tunnel

import (
	"crypto/subtle"
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

type Connection struct {
	Subdomain     string
	Conn          *websocket.Conn
	SendCh        chan []byte
	CloseCh       chan struct{}
	LastActive    time.Time
	mu            sync.RWMutex
	logger        *zap.Logger
	closed        atomic.Bool
	tunnelType    protocol.TunnelType
	tunnelTypeStr string // cached string for metrics, set once
	openStream    func() (net.Conn, error)
	remoteIP      string

	// Control plane ownership. Empty for legacy shared-token and anonymous
	// registrations, which have no identity in the database.
	clientID  string
	accountID string

	bytesIn           atomic.Int64
	bytesOut          atomic.Int64
	activeConnections atomic.Int64

	ipAccessChecker *netutil.IPAccessChecker
	proxyAuth       *protocol.ProxyAuth

	bandwidth       int64
	burstMultiplier float64
	limiter         interface{ IsLimited() bool }
}

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

func (c *Connection) Send(data []byte) error {
	if c.closed.Load() {
		return ErrConnectionClosed
	}

	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()

	select {
	case c.SendCh <- data:
		c.UpdateActivity()
		return nil
	case <-timer.C:
		return ErrSendTimeout
	}
}

func (c *Connection) UpdateActivity() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.LastActive = time.Now()
}

func (c *Connection) IsAlive(timeout time.Duration) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return time.Since(c.LastActive) < timeout
}

func (c *Connection) Close() {
	if c.closed.Swap(true) {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	close(c.CloseCh)
	close(c.SendCh)

	if c.Conn != nil {
		_ = c.Conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		_ = c.Conn.Close()
	}

	c.logger.Info("Connection closed", zap.String("subdomain", c.Subdomain))
}

func (c *Connection) IsClosed() bool {
	return c.closed.Load()
}

func (c *Connection) SetTunnelType(tType protocol.TunnelType) {
	c.mu.Lock()
	c.tunnelType = tType
	c.tunnelTypeStr = tType.String()
	c.mu.Unlock()
}

func (c *Connection) GetTunnelType() protocol.TunnelType {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.tunnelType
}

func (c *Connection) SetOpenStream(open func() (net.Conn, error)) {
	c.mu.Lock()
	c.openStream = open
	c.mu.Unlock()
}

func (c *Connection) OpenStream() (net.Conn, error) {
	if c.closed.Load() {
		return nil, ErrConnectionClosed
	}

	c.mu.RLock()
	open := c.openStream
	c.mu.RUnlock()

	if open == nil {
		return nil, ErrConnectionClosed
	}
	c.UpdateActivity()
	return open()
}

func (c *Connection) AddBytesIn(n int64) {
	if n <= 0 {
		return
	}
	c.UpdateActivity()
	c.bytesIn.Add(n)
	metrics.BytesReceived.Add(float64(n))
	metrics.TunnelBytesReceived.WithLabelValues(c.Subdomain, c.Subdomain, c.tunnelTypeStr).Add(float64(n))
}

func (c *Connection) AddBytesOut(n int64) {
	if n <= 0 {
		return
	}
	c.UpdateActivity()
	c.bytesOut.Add(n)
	metrics.BytesSent.Add(float64(n))
	metrics.TunnelBytesSent.WithLabelValues(c.Subdomain, c.Subdomain, c.tunnelTypeStr).Add(float64(n))
}

func (c *Connection) GetBytesIn() int64  { return c.bytesIn.Load() }
func (c *Connection) GetBytesOut() int64 { return c.bytesOut.Load() }

func (c *Connection) IncActiveConnections() {
	c.activeConnections.Add(1)
	metrics.TunnelActiveConnections.WithLabelValues(c.Subdomain, c.Subdomain, c.tunnelTypeStr).Inc()
}

func (c *Connection) DecActiveConnections() {
	for {
		current := c.activeConnections.Load()
		if current <= 0 {
			return
		}
		if c.activeConnections.CompareAndSwap(current, current-1) {
			metrics.TunnelActiveConnections.WithLabelValues(c.Subdomain, c.Subdomain, c.tunnelTypeStr).Dec()
			return
		}
	}
}

func (c *Connection) GetActiveConnections() int64 { return c.activeConnections.Load() }

// SetOwner records which control-plane client and account own this tunnel.
func (c *Connection) SetOwner(clientID, accountID string) {
	c.mu.Lock()
	c.clientID = clientID
	c.accountID = accountID
	c.mu.Unlock()
}

// Owner returns the control-plane client and account owning this tunnel.
func (c *Connection) Owner() (clientID, accountID string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.clientID, c.accountID
}

func (c *Connection) SetIPAccessControl(allowCIDRs, denyIPs []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ipAccessChecker = netutil.NewIPAccessChecker(allowCIDRs, denyIPs)
}

func (c *Connection) IsIPAllowed(ip string) bool {
	c.mu.RLock()
	checker := c.ipAccessChecker
	c.mu.RUnlock()

	if checker == nil {
		return true
	}
	return checker.IsAllowed(ip)
}

func (c *Connection) HasIPAccessControl() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.ipAccessChecker != nil && c.ipAccessChecker.HasRules()
}

func (c *Connection) SetProxyAuth(auth *protocol.ProxyAuth) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.proxyAuth = auth
}

func (c *Connection) GetProxyAuth() *protocol.ProxyAuth {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.proxyAuth
}

func (c *Connection) HasProxyAuth() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.proxyAuth != nil && c.proxyAuth.Enabled
}

func (c *Connection) ValidateProxyAuth(password string) bool {
	c.mu.RLock()
	auth := c.proxyAuth
	c.mu.RUnlock()

	if auth == nil || !auth.Enabled {
		return true
	}
	return subtle.ConstantTimeCompare([]byte(auth.Password), []byte(password)) == 1
}

func (c *Connection) SetBandwidthWithBurst(bandwidth int64, burstMultiplier float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bandwidth = bandwidth
	c.burstMultiplier = burstMultiplier
}

func (c *Connection) GetBandwidth() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.bandwidth
}

func (c *Connection) SetLimiter(limiter interface{ IsLimited() bool }) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.limiter = limiter
}

func (c *Connection) GetLimiter() interface{ IsLimited() bool } {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.limiter
}

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

			if err := c.Conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
				c.logger.Error("SetWriteDeadline failed", zap.String("subdomain", c.Subdomain), zap.Error(err))
				return
			}
			if err := c.Conn.WriteMessage(websocket.TextMessage, message); err != nil {
				c.logger.Error("Write error", zap.String("subdomain", c.Subdomain), zap.Error(err))
				return
			}

		case <-ticker.C:
			if err := c.Conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
				c.logger.Error("SetWriteDeadline failed", zap.String("subdomain", c.Subdomain), zap.Error(err))
				return
			}
			if err := c.Conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}

		case <-c.CloseCh:
			return
		}
	}
}
