package tcp

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	json "github.com/goccy/go-json"
	"github.com/hashicorp/yamux"
	"go.uber.org/zap"

	"drip/internal/shared/constants"
	"drip/internal/shared/mux"
	"drip/internal/shared/protocol"
	"drip/internal/shared/stats"
	"drip/pkg/config"
)

// PoolClient manages a pool of yamux sessions for tunnel connections.
type PoolClient struct {
	serverAddr string
	tlsConfig  *tls.Config
	token      string
	tunnelType protocol.TunnelType
	localHost  string
	localPort  int
	subdomain  string

	assignedURL string
	tunnelID    string

	minSessions     int
	maxSessions     int
	initialSessions int

	stats *stats.TrafficStats

	httpClient *http.Client

	latencyCallback atomic.Value // LatencyCallback
	latencyNanos    atomic.Int64

	ctx    context.Context
	cancel context.CancelFunc

	stopCh chan struct{}
	doneCh chan struct{}
	once   sync.Once
	wg     sync.WaitGroup
	closed atomic.Bool

	primary *sessionHandle

	mu              sync.RWMutex
	dataSessions    map[string]*sessionHandle
	desiredTotal    int
	lastScale       time.Time
	pendingSessions int

	logger *zap.Logger

	allowIPs []string
	denyIPs  []string

	authPass   string
	authBearer string

	// Transport protocol selection
	transport TransportType
	insecure  bool

	// Connection dialer
	dialer *ConnectionDialer

	// Session scaler
	scaler *SessionScaler

	// Bandwidth limit requested from server (bytes/sec), 0 = unlimited
	bandwidth int64

	// TLS verification behavior for local HTTPS backends
	skipLocalTLSVerify bool
}

// NewPoolClient creates a new pool client.
func NewPoolClient(cfg *ConnectorConfig, logger *zap.Logger) *PoolClient {
	// Parse server address to get host for TLS config
	serverAddr := cfg.ServerAddr
	host := serverAddr

	// Handle wss:// prefix
	if strings.HasPrefix(serverAddr, "wss://") {
		if u, err := url.Parse(serverAddr); err == nil {
			host = u.Host
			// Normalize server address for internal use
			if u.Port() == "" {
				host = u.Host + ":443"
			}
			serverAddr = host
		}
	}

	// Extract hostname without port for TLS
	hostOnly, _, _ := net.SplitHostPort(host)
	if hostOnly == "" {
		hostOnly = host
	}

	var tlsConfig *tls.Config
	if cfg.Insecure {
		tlsConfig = config.GetClientTLSConfigInsecure()
	} else {
		tlsConfig = config.GetClientTLSConfig(hostOnly)
	}

	localHost := cfg.LocalHost
	if localHost == "" {
		localHost = "127.0.0.1"
	}

	tunnelType := cfg.TunnelType
	if tunnelType == "" {
		tunnelType = protocol.TunnelTypeTCP
	}

	numCPU := runtime.NumCPU()

	minSessions := cfg.PoolMin
	if minSessions <= 0 {
		minSessions = 2
	}

	maxSessions := cfg.PoolMax
	if maxSessions <= 0 {
		maxSessions = max(numCPU*16, minSessions)
	}
	if maxSessions < minSessions {
		maxSessions = minSessions
	}

	initialSessions := cfg.PoolSize
	if initialSessions <= 0 {
		initialSessions = 4
	}
	initialSessions = min(max(initialSessions, minSessions), maxSessions)

	// Determine transport type
	transport := cfg.Transport
	if transport == "" {
		transport = TransportAuto
	}

	ctx, cancel := context.WithCancel(context.Background())

	c := &PoolClient{
		serverAddr:         serverAddr,
		tlsConfig:          tlsConfig,
		token:              cfg.Token,
		tunnelType:         tunnelType,
		localHost:          localHost,
		localPort:          cfg.LocalPort,
		subdomain:          cfg.Subdomain,
		minSessions:        minSessions,
		maxSessions:        maxSessions,
		initialSessions:    initialSessions,
		stats:              stats.NewTrafficStats(),
		ctx:                ctx,
		cancel:             cancel,
		stopCh:             make(chan struct{}),
		doneCh:             make(chan struct{}),
		dataSessions:       make(map[string]*sessionHandle),
		logger:             logger,
		allowIPs:           cfg.AllowIPs,
		denyIPs:            cfg.DenyIPs,
		authPass:           cfg.AuthPass,
		authBearer:         cfg.AuthBearer,
		transport:          transport,
		insecure:           cfg.Insecure,
		dialer:             NewConnectionDialer(serverAddr, tlsConfig, cfg.Token, transport, logger),
		bandwidth:          cfg.Bandwidth,
		skipLocalTLSVerify: cfg.SkipLocalTLSVerify || isLoopbackHost(localHost),
	}

	if tunnelType == protocol.TunnelTypeHTTP || tunnelType == protocol.TunnelTypeHTTPS {
		c.httpClient = newLocalHTTPClient(tunnelType, c.skipLocalTLSVerify)
	}

	c.latencyCallback.Store(LatencyCallback(func(time.Duration) {}))
	return c
}

func isLoopbackHost(host string) bool {
	host = strings.TrimPrefix(strings.TrimSuffix(host, "]"), "[")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// Connect establishes the primary connection and starts background workers.
func (c *PoolClient) Connect() error {
	primaryConn, err := c.dialer.Dial()
	if err != nil {
		return err
	}

	maxData := max(c.maxSessions-1, 0)
	req := protocol.RegisterRequest{
		Token:           c.token,
		CustomSubdomain: c.subdomain,
		TunnelType:      c.tunnelType,
		LocalPort:       c.localPort,
		ConnectionType:  "primary",
		PoolCapabilities: &protocol.PoolCapabilities{
			MaxDataConns: maxData,
			Version:      1,
		},
	}

	if len(c.allowIPs) > 0 || len(c.denyIPs) > 0 {
		req.IPAccess = &protocol.IPAccessControl{
			AllowIPs: c.allowIPs,
			DenyIPs:  c.denyIPs,
		}
	}

	if c.authBearer != "" {
		req.ProxyAuth = &protocol.ProxyAuth{
			Enabled: true,
			Type:    "bearer",
			Token:   c.authBearer,
		}
	} else if c.authPass != "" {
		req.ProxyAuth = &protocol.ProxyAuth{
			Enabled:  true,
			Type:     "password",
			Password: c.authPass,
		}
	}

	if c.bandwidth > 0 {
		req.Bandwidth = c.bandwidth
	}

	payload, err := json.Marshal(req)
	if err != nil {
		_ = primaryConn.Close()
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	if err := protocol.WriteFrame(primaryConn, protocol.NewFrame(protocol.FrameTypeRegister, payload)); err != nil {
		_ = primaryConn.Close()
		return fmt.Errorf("failed to send registration: %w", err)
	}

	_ = primaryConn.SetReadDeadline(time.Now().Add(constants.RequestTimeout))
	ack, err := protocol.ReadFrame(primaryConn)
	if err != nil {
		_ = primaryConn.Close()
		return fmt.Errorf("failed to read register ack: %w", err)
	}
	defer ack.Release()
	_ = primaryConn.SetReadDeadline(time.Time{})

	if ack.Type == protocol.FrameTypeError {
		var errMsg protocol.ErrorMessage
		if e := json.Unmarshal(ack.Payload, &errMsg); e == nil {
			_ = primaryConn.Close()
			return fmt.Errorf("registration error: %s - %s", errMsg.Code, errMsg.Message)
		}
		_ = primaryConn.Close()
		return fmt.Errorf("registration error")
	}
	if ack.Type != protocol.FrameTypeRegisterAck {
		_ = primaryConn.Close()
		return fmt.Errorf("unexpected register ack frame: %s", ack.Type)
	}

	var resp protocol.RegisterResponse
	if err := json.Unmarshal(ack.Payload, &resp); err != nil {
		_ = primaryConn.Close()
		return fmt.Errorf("failed to parse register response: %w", err)
	}

	c.assignedURL = resp.URL
	c.subdomain = resp.Subdomain
	if resp.SupportsDataConn && resp.TunnelID != "" {
		c.tunnelID = resp.TunnelID
	}

	if resp.Bandwidth > 0 {
		c.bandwidth = resp.Bandwidth
	}

	yamuxCfg := mux.NewClientConfig()

	session, err := yamux.Server(primaryConn, yamuxCfg)
	if err != nil {
		_ = primaryConn.Close()
		return fmt.Errorf("failed to init yamux session: %w", err)
	}

	primary := &sessionHandle{
		id:      "primary",
		conn:    primaryConn,
		session: session,
	}
	primary.touch()
	c.mu.Lock()
	c.primary = primary
	c.mu.Unlock()

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		<-c.stopCh
	}()

	c.wg.Add(1)
	go c.acceptLoop(primary, true)

	c.wg.Add(1)
	go c.sessionWatcher(primary, true)

	c.wg.Add(1)
	go c.pingLoop(primary)

	if c.tunnelID != "" {
		c.mu.Lock()
		c.desiredTotal = c.initialSessions
		c.mu.Unlock()

		c.warmupSessions()

		// Initialize and start session scaler
		c.scaler = NewSessionScaler(c, c.logger, c.stopCh, &c.wg)
		c.scaler.Start()
	}

	go func() {
		c.wg.Wait()
		close(c.doneCh)
	}()

	return nil
}

func (c *PoolClient) acceptLoop(h *sessionHandle, isPrimary bool) {
	defer c.wg.Done()

	for {
		select {
		case <-c.stopCh:
			return
		default:
		}

		stream, err := h.session.Accept()
		if err != nil {
			if c.IsClosed() || isExpectedCloseError(err) {
				return
			}
			if isPrimary {
				c.logger.Debug("Primary session accept failed", zap.Error(err))
				_ = c.Close()
				return
			}

			c.logger.Debug("Data session accept failed", zap.String("session_id", h.id), zap.Error(err))
			c.removeDataSession(h.id)
			return
		}

		h.active.Add(1)
		h.touch()

		c.stats.AddRequest()
		c.stats.IncActiveConnections()

		c.wg.Add(1)
		go c.handleStream(h, stream)
	}
}

func (c *PoolClient) sessionWatcher(h *sessionHandle, isPrimary bool) {
	defer c.wg.Done()

	select {
	case <-c.stopCh:
		return
	case <-h.session.CloseChan():
		if isPrimary {
			_ = c.Close()
			return
		}
		c.removeDataSession(h.id)
	}
}

func (c *PoolClient) pingLoop(h *sessionHandle) {
	defer c.wg.Done()

	const maxConsecutiveFailures = 3

	ticker := time.NewTicker(constants.HeartbeatInterval)
	defer ticker.Stop()

	consecutiveFailures := 0

	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
		}

		if h.session == nil || h.session.IsClosed() {
			return
		}

		latency, err := h.session.Ping()
		if err != nil {
			consecutiveFailures++
			c.logger.Debug("Ping failed",
				zap.String("session_id", h.id),
				zap.Int("consecutive_failures", consecutiveFailures),
				zap.Error(err),
			)

			if consecutiveFailures >= maxConsecutiveFailures {
				c.logger.Warn("Session ping failed too many times, closing",
					zap.String("session_id", h.id),
					zap.Int("failures", consecutiveFailures),
				)
				if h.id == "primary" {
					_ = c.Close()
					return
				}
				c.removeDataSession(h.id)
				return
			}
			continue
		}

		consecutiveFailures = 0
		h.touch()

		c.latencyNanos.Store(int64(latency))
		if cb, ok := c.latencyCallback.Load().(LatencyCallback); ok && cb != nil {
			cb(latency)
		}
	}
}

// Close shuts down the client and all sessions.
func (c *PoolClient) Close() error {
	var closeErr error

	c.once.Do(func() {
		c.closed.Store(true)
		close(c.stopCh)

		if c.cancel != nil {
			c.cancel()
		}

		var data []*sessionHandle
		var primary *sessionHandle

		c.mu.Lock()
		for _, h := range c.dataSessions {
			data = append(data, h)
		}
		c.dataSessions = make(map[string]*sessionHandle)
		primary = c.primary
		c.primary = nil
		c.mu.Unlock()

		for _, h := range data {
			if h == nil {
				continue
			}
			if h.session != nil {
				_ = h.session.Close()
			}
			if h.conn != nil {
				_ = h.conn.Close()
			}
		}

		if primary != nil {
			if primary.session != nil {
				closeErr = primary.session.Close()
			}
			if primary.conn != nil {
				_ = primary.conn.Close()
			}
		}
	})

	return closeErr
}

func (c *PoolClient) Wait()                         { <-c.doneCh }
func (c *PoolClient) GetURL() string                { return c.assignedURL }
func (c *PoolClient) GetSubdomain() string          { return c.subdomain }
func (c *PoolClient) GetLatency() time.Duration     { return time.Duration(c.latencyNanos.Load()) }
func (c *PoolClient) GetStats() *stats.TrafficStats { return c.stats }
func (c *PoolClient) IsClosed() bool                { return c.closed.Load() }

func (c *PoolClient) SetLatencyCallback(cb LatencyCallback) {
	if cb == nil {
		cb = func(time.Duration) {}
	}
	c.latencyCallback.Store(cb)
}
