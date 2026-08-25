package tcp

import (
	"bufio"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	json "github.com/goccy/go-json"
	"github.com/hashicorp/yamux"

	"drip/internal/server/auth"
	"drip/internal/server/reservations"
	"drip/internal/server/tunnel"
	"drip/internal/shared/constants"
	"drip/internal/shared/httputil"
	"drip/internal/shared/protocol"
	"drip/internal/shared/qos"
	"drip/internal/shared/units"
	"drip/internal/shared/utils"

	"go.uber.org/zap"
)

type ConnectionConfig struct {
	Conn net.Conn
	// AuthToken is the legacy shared server token. Prefer Authenticator.
	AuthToken string
	// Authenticator resolves registration tokens to control-plane identities.
	// Nil falls back to the legacy AuthToken comparison.
	Authenticator *auth.Authenticator
	// Resolver applies tunnel reservation policy. Nil means every registration
	// resolves to an ephemeral tunnel.
	Resolver     *reservations.Resolver
	Manager      *tunnel.Manager
	Logger       *zap.Logger
	PortAlloc    *PortAllocator
	Domain       string
	TunnelDomain string
	PublicPort   int
	HTTPHandler  http.Handler
	GroupManager *ConnectionGroupManager
	HTTPListener *connQueueListener
	RemoteIP     string
}

type Connection struct {
	conn             net.Conn
	authToken        string
	authenticator    *auth.Authenticator
	identity         *auth.Identity
	resolver         *reservations.Resolver
	manager          *tunnel.Manager
	logger           *zap.Logger
	subdomain        string
	port             int
	domain           string
	tunnelDomain     string
	publicPort       int
	portAlloc        *PortAllocator
	tunnelConn       *tunnel.Connection
	stopCh           chan struct{}
	once             sync.Once
	lastHeartbeat    time.Time
	mu               sync.RWMutex
	frameWriter      *protocol.FrameWriter
	httpHandler      http.Handler
	tunnelType       protocol.TunnelType
	ctx              context.Context
	cancel           context.CancelFunc
	session          *yamux.Session
	proxy            *Proxy
	tunnelID         string
	groupManager     *ConnectionGroupManager
	httpListener     *connQueueListener
	handedOff        bool
	lifecycleManager *ConnectionLifecycleManager

	// Server capabilities
	allowedTunnelTypes []string
	allowedTransports  []string
	bandwidth          int64
	burstMultiplier    float64
	remoteIP           string
}

// NewConnection creates a new connection handler
func NewConnection(cfg ConnectionConfig) *Connection {
	ctx, cancel := context.WithCancel(context.Background())
	stopCh := make(chan struct{})

	c := &Connection{
		conn:             cfg.Conn,
		authToken:        cfg.AuthToken,
		authenticator:    cfg.Authenticator,
		resolver:         cfg.Resolver,
		manager:          cfg.Manager,
		logger:           cfg.Logger,
		portAlloc:        cfg.PortAlloc,
		domain:           cfg.Domain,
		tunnelDomain:     cfg.TunnelDomain,
		publicPort:       cfg.PublicPort,
		httpHandler:      cfg.HTTPHandler,
		stopCh:           stopCh,
		lastHeartbeat:    time.Now(),
		ctx:              ctx,
		cancel:           cancel,
		groupManager:     cfg.GroupManager,
		httpListener:     cfg.HTTPListener,
		lifecycleManager: NewConnectionLifecycleManager(stopCh, cancel, cfg.Logger),
		remoteIP:         cfg.RemoteIP,
	}

	// Set connection in lifecycle manager
	c.lifecycleManager.SetConnection(cfg.Conn)

	return c
}

// authenticate resolves the registration token to an identity. When no
// Authenticator is configured the legacy shared-token comparison applies, which
// keeps existing self-hosted deployments working unchanged.
func (c *Connection) authenticate(token string) (*auth.Identity, error) {
	if c.authenticator != nil {
		ctx, cancel := context.WithTimeout(c.ctx, 5*time.Second)
		defer cancel()
		return c.authenticator.Authenticate(ctx, token, c.remoteIP)
	}

	if c.authToken != "" && subtle.ConstantTimeCompare([]byte(token), []byte(c.authToken)) != 1 {
		return nil, auth.ErrInvalidCredential
	}
	if c.authToken != "" {
		return &auth.Identity{Legacy: true}, nil
	}
	return &auth.Identity{Anonymous: true}, nil
}

// owner converts the authenticated identity into a tunnel.Owner.
func (c *Connection) owner() tunnel.Owner {
	if c.identity == nil || !c.identity.IsStored() {
		return tunnel.Owner{}
	}
	owner := tunnel.Owner{
		ClientID:  c.identity.Client.ID,
		AccountID: c.identity.Client.AccountID,
	}
	if c.identity.Account != nil {
		owner.MaxTunnels = c.identity.Account.MaxTunnels
	}
	return owner
}

func (c *Connection) Handle() error {
	protocol.RegisterConnection()
	defer c.Close()

	if err := c.conn.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
		c.logger.Warn("Failed to set read deadline", zap.Error(err))
	}
	reader := bufio.NewReader(c.conn)

	peek, err := reader.Peek(4)
	if err != nil {
		return fmt.Errorf("failed to peek connection: %w", err)
	}

	if httputil.IsHTTPRequest(peek) {
		c.logger.Info("Detected HTTP request on TCP port, handling as HTTP")
		return c.handleHTTPRequest(reader)
	}

	// Check if TCP transport is allowed (only for Drip protocol connections, not HTTP)
	if !c.isTransportAllowed("tcp") {
		c.logger.Warn("TCP transport not allowed, rejecting Drip protocol connection")
		return fmt.Errorf("TCP transport not allowed")
	}

	frame, err := protocol.ReadFrame(reader)
	if err != nil {
		return fmt.Errorf("failed to read registration frame: %w", err)
	}
	sf := protocol.WithFrame(frame)
	defer sf.Close()

	if sf.Frame.Type == protocol.FrameTypeDataConnect {
		handler := NewDataConnectionHandler(
			c.conn,
			reader,
			c.authToken,
			c.groupManager,
			c.stopCh,
			c.logger,
		)
		handler.SetSessionCreatedHandler(func(session *yamux.Session) {
			c.session = session
			if c.lifecycleManager != nil {
				c.lifecycleManager.SetSession(session)
			}
		})
		handler.SetTunnelIDHandler(func(tunnelID string) {
			c.tunnelID = tunnelID
		})
		return handler.Handle(sf.Frame)
	}

	if sf.Frame.Type != protocol.FrameTypeRegister {
		return fmt.Errorf("expected register frame, got %s", sf.Frame.Type)
	}

	var req protocol.RegisterRequest
	if err := json.Unmarshal(sf.Frame.Payload, &req); err != nil {
		return fmt.Errorf("failed to parse registration request: %w", err)
	}

	c.tunnelType = req.TunnelType

	// Check if tunnel type is allowed
	if !c.isTunnelTypeAllowed(string(req.TunnelType)) {
		c.sendError("tunnel_type_not_allowed", fmt.Sprintf("Tunnel type '%s' is not allowed on this server", req.TunnelType))
		return fmt.Errorf("tunnel type not allowed: %s", req.TunnelType)
	}

	identity, err := c.authenticate(req.Token)
	if err != nil {
		// The client is told only that authentication failed; the specific
		// reason stays server-side so a probe cannot enumerate credentials.
		c.sendError("authentication_failed", "Invalid authentication token")
		c.logger.Warn("Registration authentication failed",
			zap.String("remote_ip", c.remoteIP),
			zap.Error(err),
		)
		return fmt.Errorf("authentication failed: %w", err)
	}
	c.identity = identity

	// Use RegistrationHandler for registration logic
	regHandler := NewRegistrationHandler(
		c.manager,
		c.portAlloc,
		c.groupManager,
		c.domain,
		c.tunnelDomain,
		c.publicPort,
		c.logger,
	)
	regHandler.SetResolver(c.resolver)

	regReq := &RegistrationRequest{
		TunnelType:       req.TunnelType,
		CustomSubdomain:  req.CustomSubdomain,
		Token:            req.Token,
		ConnectionType:   req.ConnectionType,
		PoolCapabilities: req.PoolCapabilities,
		IPAccess:         req.IPAccess,
		ProxyAuth:        req.ProxyAuth,
		LocalPort:        req.LocalPort,
		RemoteIP:         c.remoteIP,
		Owner:            c.owner(),
	}

	result, err := regHandler.Register(c.ctx, regReq)
	if err != nil {
		c.sendError("registration_failed", err.Error())
		return fmt.Errorf("registration failed: %w", err)
	}

	// Store registration results
	c.subdomain = result.Subdomain
	c.port = result.Port
	c.tunnelConn = result.TunnelConn
	// The tunnel connection is registered with a nil websocket (this is the raw
	// TCP path), so Conn is already nil. Assigning it here again would race the
	// write pump goroutine the manager starts at registration.

	// Update lifecycle manager with registration info
	if c.lifecycleManager != nil {
		c.lifecycleManager.SetPortAllocation(c.portAlloc, c.port)
		c.lifecycleManager.SetTunnelRegistration(c.manager, c.subdomain, "", c.groupManager)
	}

	// Handle connection groups
	if result.SupportsDataConn && c.groupManager != nil {
		group := c.groupManager.CreateGroup(result.Subdomain, req.Token, c, req.TunnelType)
		result.TunnelID = group.TunnelID
		c.tunnelID = result.TunnelID

		// Update lifecycle manager with tunnel ID
		if c.lifecycleManager != nil {
			c.lifecycleManager.SetTunnelRegistration(c.manager, c.subdomain, c.tunnelID, c.groupManager)
		}

		c.logger.Info("Created connection group for multi-connection support",
			zap.String("tunnel_id", result.TunnelID),
			zap.Int("max_data_conns", req.PoolCapabilities.MaxDataConns),
		)
	}

	// Configure bandwidth limiting. The effective limit is the smallest of the
	// server default, the reservation override and whatever the client asked
	// for, so no party can raise a limit another one set.
	effectiveBandwidth := c.bandwidth
	if result.Bandwidth != "" {
		reserved, berr := units.ParseBandwidth(result.Bandwidth)
		if berr != nil {
			c.logger.Warn("Ignoring invalid reservation bandwidth",
				zap.String("subdomain", c.subdomain),
				zap.String("bandwidth", result.Bandwidth),
				zap.Error(berr),
			)
		} else if reserved > 0 && (effectiveBandwidth == 0 || reserved < effectiveBandwidth) {
			effectiveBandwidth = reserved
		}
	}
	if req.Bandwidth > 0 {
		if effectiveBandwidth == 0 || req.Bandwidth < effectiveBandwidth {
			effectiveBandwidth = req.Bandwidth
		}
	}
	// Clamp client-supplied bandwidth to reasonable maximum (1GB/s) to prevent bypass
	const maxClientBandwidth int64 = 1 << 30
	if effectiveBandwidth > maxClientBandwidth {
		effectiveBandwidth = maxClientBandwidth
	}
	if effectiveBandwidth > 0 {
		burstMultiplier := c.burstMultiplier
		if burstMultiplier <= 0 {
			burstMultiplier = 2.0
		}
		c.tunnelConn.SetBandwidthWithBurst(effectiveBandwidth, burstMultiplier)
		burst := limiterBurst(effectiveBandwidth, burstMultiplier)

		limiter := qos.NewLimiter(qos.Config{
			Bandwidth: effectiveBandwidth,
			Burst:     burst,
		})
		c.tunnelConn.SetLimiter(limiter)

		source := "server"
		if req.Bandwidth > 0 && (c.bandwidth == 0 || req.Bandwidth < c.bandwidth) {
			source = "client"
		}
		c.logger.Info("Bandwidth limit configured",
			zap.String("subdomain", c.subdomain),
			zap.Int64("bandwidth_bytes_sec", effectiveBandwidth),
			zap.Float64("burst_multiplier", burstMultiplier),
			zap.Int("burst_bytes", burst),
			zap.String("source", source),
		)
	}

	// Build and send registration response
	resp, err := regHandler.BuildRegistrationResponse(result)
	if err != nil {
		return fmt.Errorf("failed to build registration response: %w", err)
	}
	resp.Bandwidth = c.tunnelConn.GetBandwidth()

	if err := regHandler.SendRegistrationResponse(c.conn, resp); err != nil {
		return fmt.Errorf("failed to send registration ack: %w", err)
	}

	if err := c.conn.SetReadDeadline(time.Time{}); err != nil {
		c.logger.Warn("Failed to clear read deadline", zap.Error(err))
	}

	if req.TunnelType == protocol.TunnelTypeTCP {
		return c.handleTCPTunnel(reader)
	}
	if req.TunnelType == protocol.TunnelTypeHTTP || req.TunnelType == protocol.TunnelTypeHTTPS {
		return c.handleHTTPProxyTunnel(reader)
	}

	c.frameWriter = protocol.NewFrameWriter(c.conn)

	// Update lifecycle manager with frame writer
	if c.lifecycleManager != nil {
		c.lifecycleManager.SetFrameWriter(c.frameWriter)
	}

	c.frameWriter.SetWriteErrorHandler(func(err error) {
		c.logger.Error("Write error detected, closing connection", zap.Error(err))
		c.Close()
	})

	go c.heartbeatChecker()

	// Use FrameHandler for frame processing
	frameHandler := NewFrameHandler(c.conn, reader, c.stopCh, c.frameWriter, c.logger)
	frameHandler.SetHeartbeatHandler(func() {
		c.handleHeartbeat()
	})
	frameHandler.SetCloseHandler(func() {
		c.logger.Info("Client requested close")
	})

	return frameHandler.HandleFrames()
}

func (c *Connection) handleHTTPRequest(reader *bufio.Reader) error {
	handler := NewHTTPRequestHandler(
		c.conn,
		reader,
		c.httpHandler,
		c.httpListener,
		c.ctx,
		c.logger,
		&c.mu,
		&c.handedOff,
	)
	return handler.Handle()
}

func (c *Connection) IsHandedOff() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.handedOff
}

func parseTCPSubdomainPort(subdomain string) (int, bool) {
	if !strings.HasPrefix(subdomain, "tcp-") {
		return 0, false
	}

	portStr := strings.TrimPrefix(subdomain, "tcp-")
	if portStr == "" {
		return 0, false
	}

	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return 0, false
	}

	return port, true
}

func (c *Connection) handleHeartbeat() {
	c.mu.Lock()
	c.lastHeartbeat = time.Now()
	c.mu.Unlock()

	ackFrame := protocol.NewFrame(protocol.FrameTypeHeartbeatAck, nil)
	err := c.frameWriter.WriteControl(ackFrame)
	if err != nil {
		c.logger.Error("Failed to send heartbeat ack", zap.Error(err))
	}
}

func (c *Connection) heartbeatChecker() {
	ticker := time.NewTicker(constants.HeartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
			c.mu.RLock()
			lastHB := c.lastHeartbeat
			c.mu.RUnlock()

			if time.Since(lastHB) > constants.HeartbeatTimeout {
				c.logger.Warn("Heartbeat timeout",
					zap.String("subdomain", c.subdomain),
					zap.Duration("last_heartbeat", time.Since(lastHB)),
				)
				c.Close()
				return
			}
		}
	}
}

func (c *Connection) sendError(code, message string) {
	errMsg := protocol.ErrorMessage{
		Code:    code,
		Message: message,
	}
	data, err := json.Marshal(errMsg)
	if err != nil {
		c.logger.Error("Failed to marshal error message", zap.Error(err))
		return
	}
	errFrame := protocol.NewFrame(protocol.FrameTypeError, data)

	if c.frameWriter == nil {
		_ = protocol.WriteFrame(c.conn, errFrame)
	} else {
		_ = c.frameWriter.WriteFrame(errFrame)
	}
}

func (c *Connection) Close() {
	c.once.Do(func() {
		// Check if connection was handed off to HTTP handler
		c.mu.RLock()
		handedOff := c.handedOff
		c.mu.RUnlock()

		// If handed off, don't close the connection - HTTP handler owns it now
		if handedOff {
			protocol.UnregisterConnection()
			c.logger.Debug("Connection handed off to HTTP handler, skipping close")
			return
		}

		// Use lifecycle manager for cleanup
		if c.lifecycleManager != nil {
			c.lifecycleManager.Close()
		} else {
			// Fallback if lifecycle manager not initialized
			protocol.UnregisterConnection()
			close(c.stopCh)

			if c.cancel != nil {
				c.cancel()
			}

			c.mu.Lock()
			conn := c.conn
			c.mu.Unlock()

			if conn != nil {
				_ = conn.SetDeadline(time.Now())
			}

			if c.frameWriter != nil {
				_ = c.frameWriter.Close()
			}

			if c.proxy != nil {
				c.proxy.Stop()
			}

			if c.session != nil {
				_ = c.session.Close()
			}

			if conn != nil {
				_ = conn.Close()
			}

			if c.port > 0 && c.portAlloc != nil {
				c.portAlloc.Release(c.port)
			}

			if c.subdomain != "" {
				c.manager.Unregister(c.subdomain)
				if c.tunnelID != "" && c.groupManager != nil {
					c.groupManager.RemoveGroup(c.tunnelID)
				}
			}

			c.logger.Info("Connection closed",
				zap.String("subdomain", c.subdomain),
			)
		}
	})
}

func isTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return strings.Contains(err.Error(), "i/o timeout")
}

// SetAllowedTunnelTypes sets the allowed tunnel types for this connection
func (c *Connection) SetAllowedTunnelTypes(types []string) {
	c.allowedTunnelTypes = types
}

// SetAllowedTransports sets the allowed transports for this connection
func (c *Connection) SetAllowedTransports(transports []string) {
	c.allowedTransports = transports
}

// isTransportAllowed checks if a transport is allowed
func (c *Connection) isTransportAllowed(transport string) bool {
	return utils.ContainsIgnoreCase(c.allowedTransports, transport)
}

// isTunnelTypeAllowed checks if a tunnel type is allowed
func (c *Connection) isTunnelTypeAllowed(tunnelType string) bool {
	return utils.ContainsIgnoreCase(c.allowedTunnelTypes, tunnelType)
}

func (c *Connection) SetBandwidthConfig(bandwidth int64, burstMultiplier float64) {
	c.bandwidth = bandwidth
	if burstMultiplier <= 0 {
		burstMultiplier = 2.0
	}
	c.burstMultiplier = burstMultiplier
}

func limiterBurst(bandwidth int64, burstMultiplier float64) int {
	if bandwidth <= 0 {
		return 0
	}

	if burstMultiplier <= 0 || math.IsNaN(burstMultiplier) || math.IsInf(burstMultiplier, 0) {
		burstMultiplier = 2.0
	}

	maxBurst := int64(^uint(0) >> 1)
	rawBurst := float64(bandwidth) * burstMultiplier
	if math.IsNaN(rawBurst) || rawBurst <= 0 {
		return 1
	}
	if rawBurst >= float64(maxBurst) {
		return int(maxBurst)
	}

	burst := int(rawBurst)
	if burst <= 0 {
		return 1
	}
	return burst
}
