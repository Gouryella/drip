package tcp

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"drip/internal/server/auth"
	"drip/internal/server/metrics"
	"drip/internal/server/proxy"
	"drip/internal/server/reservations"
	"drip/internal/server/tunnel"
	"drip/internal/shared/netutil"
	"drip/internal/shared/pool"
	"drip/internal/shared/recovery"
	"drip/internal/shared/utils"

	"go.uber.org/zap"
	"golang.org/x/net/http2"
)

type ListenerConfig struct {
	Address   string
	TLSConfig *tls.Config
	// AuthToken is the legacy shared server token. Prefer Authenticator.
	AuthToken string
	// Authenticator resolves registration tokens to control-plane identities.
	Authenticator *auth.Authenticator
	// Resolver applies tunnel reservation policy.
	Resolver     *reservations.Resolver
	Manager      *tunnel.Manager
	Logger       *zap.Logger
	PortAlloc    *PortAllocator
	Domain       string
	TunnelDomain string
	PublicPort   int
	HTTPHandler  http.Handler
}

type Listener struct {
	address       string
	tlsConfig     *tls.Config
	authToken     string
	authenticator *auth.Authenticator
	resolver      *reservations.Resolver
	manager       *tunnel.Manager
	portAlloc     *PortAllocator
	logger        *zap.Logger
	domain        string
	tunnelDomain  string
	publicPort    int
	httpHandler   http.Handler
	listener      net.Listener
	stopCh        chan struct{}
	stopOnce      sync.Once
	wg            sync.WaitGroup
	connections   sync.Map // map[string]*Connection, sync.Map for better concurrent read performance
	connCount     atomic.Int64
	connIDSeq     atomic.Int64  // unique connection ID sequence
	connSem       chan struct{} // semaphore to limit max connections
	workerPool    *pool.WorkerPool
	recoverer     *recovery.Recoverer
	panicMetrics  *recovery.PanicMetrics
	groupManager  *ConnectionGroupManager
	httpServer    *http.Server
	httpListener  *connQueueListener

	// Server capabilities
	allowedTransports  []string
	allowedTunnelTypes []string
	bandwidth          int64
	burstMultiplier    float64
}

const maxConns = 10000

func NewListener(cfg ListenerConfig) *Listener {
	numCPU := pool.NumCPU()
	workers := numCPU * 8
	queueSize := workers * 50
	workerPool := pool.NewWorkerPool(workers, queueSize)

	cfg.Logger.Info("Worker pool configured",
		zap.Int("cpu_cores", numCPU),
		zap.Int("workers", workers),
		zap.Int("queue_size", queueSize),
	)

	panicMetrics := recovery.NewPanicMetrics(cfg.Logger, nil)
	recoverer := recovery.NewRecoverer(cfg.Logger, panicMetrics)

	// Initialize worker pool metrics
	metrics.WorkerPoolSize.Set(float64(workers))

	l := &Listener{
		address:       cfg.Address,
		tlsConfig:     cfg.TLSConfig,
		authToken:     cfg.AuthToken,
		authenticator: cfg.Authenticator,
		resolver:      cfg.Resolver,
		manager:       cfg.Manager,
		portAlloc:     cfg.PortAlloc,
		logger:        cfg.Logger,
		domain:        cfg.Domain,
		tunnelDomain:  cfg.TunnelDomain,
		publicPort:    cfg.PublicPort,
		httpHandler:   cfg.HTTPHandler,
		stopCh:        make(chan struct{}),
		connSem:       make(chan struct{}, maxConns),
		workerPool:    workerPool,
		recoverer:     recoverer,
		panicMetrics:  panicMetrics,
		groupManager:  NewConnectionGroupManager(cfg.Logger),
	}

	// Set up WebSocket connection handler if httpHandler supports it
	if h, ok := cfg.HTTPHandler.(*proxy.Handler); ok {
		h.SetWSConnectionHandler(l)
		h.SetPublicPort(cfg.PublicPort)
	}

	return l
}

func (l *Listener) Start() error {
	var err error

	// Support both TLS and plain TCP modes
	if l.tlsConfig != nil {
		l.listener, err = tls.Listen("tcp", l.address, l.tlsConfig)
		if err != nil {
			return fmt.Errorf("failed to start TLS listener: %w", err)
		}
		l.logger.Info("TCP listener started (TLS mode)",
			zap.String("address", l.address),
			zap.String("tls_version", "TLS 1.3"),
		)
	} else {
		l.listener, err = net.Listen("tcp", l.address)
		if err != nil {
			return fmt.Errorf("failed to start TCP listener: %w", err)
		}
		l.logger.Info("TCP listener started (plain mode - for reverse proxy)",
			zap.String("address", l.address),
		)
	}

	l.httpListener = newConnQueueListener(l.listener.Addr(), 4096)

	l.httpServer = &http.Server{
		Handler:           l.httpHandler,
		ReadHeaderTimeout: 10 * time.Second,  // Time to read request headers
		ReadTimeout:       30 * time.Second,  // Total time to read request (prevents slow-loris)
		WriteTimeout:      60 * time.Second,  // Time to write response (allows large responses)
		IdleTimeout:       120 * time.Second, // Keep-alive timeout
		MaxHeaderBytes:    1 << 18,           // 256KB max header size (reduced from 1MB)
	}

	if err := http2.ConfigureServer(l.httpServer, &http2.Server{
		MaxConcurrentStreams:         1000,
		IdleTimeout:                  120 * time.Second,
		MaxUploadBufferPerConnection: 1 << 20, // 1MB (default 64KB)
		MaxUploadBufferPerStream:     1 << 20, // 1MB (default 64KB)
	}); err != nil {
		l.logger.Warn("Failed to configure HTTP/2", zap.Error(err))
	}

	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		l.logger.Info("HTTP server started (with context cancellation support)")
		if err := l.httpServer.Serve(l.httpListener); err != nil && err != http.ErrServerClosed {
			l.logger.Error("HTTP server error", zap.Error(err))
		}
	}()

	l.wg.Add(1)
	go l.acceptLoop()

	return nil
}

func (l *Listener) acceptLoop() {
	defer l.wg.Done()
	defer l.recoverer.Recover("acceptLoop")

	for {
		select {
		case <-l.stopCh:
			return
		default:
		}

		if tcpListener, ok := l.listener.(*net.TCPListener); ok {
			_ = tcpListener.SetDeadline(time.Now().Add(1 * time.Second))
		}

		conn, err := l.listener.Accept()
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			select {
			case <-l.stopCh:
				return
			default:
				l.logger.Error("Failed to accept connection", zap.Error(err))
				continue
			}
		}

		// Check connection limit
		select {
		case l.connSem <- struct{}{}:
		default:
			l.logger.Warn("Connection limit reached, rejecting connection",
				zap.String("remote_addr", conn.RemoteAddr().String()),
				zap.Int64("max_conns", maxConns),
			)
			_ = conn.Close()
			continue
		}

		l.wg.Add(1)
		connAddr := conn.RemoteAddr().String()
		submitted := l.workerPool.Submit(l.recoverer.WrapGoroutine(
			fmt.Sprintf("handleConnection-%s", connAddr),
			func() {
				l.handleConnection(conn)
			},
		))

		if !submitted {
			l.logger.Warn("Worker pool full, rejecting connection",
				zap.String("remote_addr", connAddr),
			)
			l.wg.Done()
			_ = conn.Close()
			<-l.connSem
		}
	}
}

func (l *Listener) handleConnection(netConn net.Conn) {
	defer l.wg.Done()
	defer func() { <-l.connSem }() // release connection slot
	remoteAddr := netConn.RemoteAddr().String()
	connID := fmt.Sprintf("%s#%d", remoteAddr, l.connIDSeq.Add(1))
	defer l.recoverer.Recover("handleConnection")

	cleanupRegistered := false
	defer func() {
		if !cleanupRegistered {
			_ = netConn.Close()
		}
	}()

	// Handle TLS connections
	if tlsConn, ok := netConn.(*tls.Conn); ok {
		if err := tlsConn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
			l.logger.Warn("Failed to set read deadline",
				zap.String("remote_addr", remoteAddr),
				zap.Error(err),
			)
			return
		}

		if err := tlsConn.Handshake(); err != nil {
			l.logger.Warn("TLS handshake failed",
				zap.String("remote_addr", remoteAddr),
				zap.Error(err),
			)
			return
		}

		if err := tlsConn.SetReadDeadline(time.Time{}); err != nil {
			l.logger.Warn("Failed to clear read deadline",
				zap.String("remote_addr", remoteAddr),
				zap.Error(err),
			)
			return
		}

		if tcpConn, ok := tlsConn.NetConn().(*net.TCPConn); ok {
			_ = tcpConn.SetNoDelay(true)
			_ = tcpConn.SetKeepAlive(true)
			_ = tcpConn.SetKeepAlivePeriod(30 * time.Second)
			_ = tcpConn.SetReadBuffer(512 * 1024)
			_ = tcpConn.SetWriteBuffer(512 * 1024)
		}

		state := tlsConn.ConnectionState()
		l.logger.Debug("New TLS connection",
			zap.String("remote_addr", remoteAddr),
			zap.Uint16("tls_version", state.Version),
			zap.String("cipher_suite", tls.CipherSuiteName(state.CipherSuite)),
		)

		if state.Version != tls.VersionTLS13 {
			l.logger.Warn("Connection not using TLS 1.3",
				zap.Uint16("version", state.Version),
			)
			return
		}
	} else {
		// Handle plain TCP connections (reverse proxy mode)
		if tcpConn, ok := netConn.(*net.TCPConn); ok {
			_ = tcpConn.SetNoDelay(true)
			_ = tcpConn.SetKeepAlive(true)
			_ = tcpConn.SetKeepAlivePeriod(30 * time.Second)
			_ = tcpConn.SetReadBuffer(512 * 1024)
			_ = tcpConn.SetWriteBuffer(512 * 1024)
		}

		l.logger.Debug("New plain TCP connection (reverse proxy mode)",
			zap.String("remote_addr", remoteAddr),
		)
	}

	remoteIP := netutil.ExtractIP(remoteAddr)
	if netutil.IsPrivateIP(remoteIP) {
		remoteIP = ""
	}

	conn := NewConnection(ConnectionConfig{
		Conn:          netConn,
		AuthToken:     l.authToken,
		Authenticator: l.authenticator,
		Resolver:      l.resolver,
		Manager:       l.manager,
		Logger:        l.logger,
		PortAlloc:     l.portAlloc,
		Domain:        l.domain,
		TunnelDomain:  l.tunnelDomain,
		PublicPort:    l.publicPort,
		HTTPHandler:   l.httpHandler,
		GroupManager:  l.groupManager,
		HTTPListener:  l.httpListener,
		RemoteIP:      remoteIP,
	})
	conn.SetAllowedTunnelTypes(l.allowedTunnelTypes)
	conn.SetAllowedTransports(l.allowedTransports)
	conn.SetBandwidthConfig(l.bandwidth, l.burstMultiplier)

	l.connections.Store(connID, conn)
	l.connCount.Add(1)

	// Update connection metrics
	metrics.TotalConnections.Inc()
	metrics.ActiveConnections.Inc()

	defer func() {
		l.connections.Delete(connID)
		l.connCount.Add(-1)

		metrics.ActiveConnections.Dec()

		if !conn.IsHandedOff() {
			_ = netConn.Close()
		}
	}()
	cleanupRegistered = true

	if err := conn.Handle(); err != nil {
		errStr := err.Error()

		if utils.IsNetworkError(errStr) {
			return
		}

		if utils.IsProtocolError(errStr) {
			l.logger.Warn("Protocol validation failed",
				zap.String("remote_addr", remoteAddr),
				zap.Error(err),
			)
		} else {
			l.logger.Error("Connection handling failed",
				zap.String("remote_addr", remoteAddr),
				zap.Error(err),
			)
		}
	}
}

func (l *Listener) Stop() error {
	l.stopOnce.Do(func() {
		l.logger.Info("Stopping TCP listener")

		close(l.stopCh)

		if l.httpServer != nil {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := l.httpServer.Shutdown(shutdownCtx); err != nil {
				l.logger.Warn("HTTP server shutdown error", zap.Error(err))
			}
			l.logger.Info("HTTP server shutdown complete")
		}

		if l.httpListener != nil {
			_ = l.httpListener.Close()
		}

		if l.listener != nil {
			if err := l.listener.Close(); err != nil {
				l.logger.Error("Failed to close listener", zap.Error(err))
			}
		}

		l.connections.Range(func(key, value interface{}) bool {
			value.(*Connection).Close()
			return true
		})

		l.wg.Wait()

		if l.workerPool != nil {
			l.workerPool.Close()
		}

		if l.groupManager != nil {
			l.groupManager.Close()
		}

		l.logger.Info("TCP listener stopped")
	})

	return nil
}

// Addr returns the address the listener is bound to, or nil before Start.
func (l *Listener) Addr() net.Addr {
	if l.listener == nil {
		return nil
	}
	return l.listener.Addr()
}

func (l *Listener) GetActiveConnections() int {
	return int(l.connCount.Load())
}

// HandleWSConnection implements proxy.WSConnectionHandler for WebSocket tunnel connections
func (l *Listener) HandleWSConnection(conn net.Conn, remoteAddr string) {
	// Enforce connection limit for WebSocket connections too
	select {
	case l.connSem <- struct{}{}:
	default:
		l.logger.Warn("Connection limit reached, rejecting WebSocket connection",
			zap.String("remote_addr", remoteAddr),
		)
		_ = conn.Close()
		return
	}

	l.wg.Add(1)
	defer l.wg.Done()
	defer func() { <-l.connSem }()

	connAddr := conn.RemoteAddr().String()
	displayAddr := remoteAddr
	if displayAddr == "" {
		displayAddr = connAddr
	}
	connID := fmt.Sprintf("%s#%d", displayAddr, l.connIDSeq.Add(1))

	l.logger.Info("Handling WebSocket tunnel connection",
		zap.String("remote_addr", connID),
	)

	remoteIP := netutil.ExtractIP(remoteAddr)
	if remoteIP == "" {
		remoteIP = netutil.ExtractIP(connAddr)
	}
	if netutil.IsPrivateIP(remoteIP) {
		remoteIP = ""
	}

	// Create connection handler (no TLS verification needed - already done by HTTP server)
	tcpConn := NewConnection(ConnectionConfig{
		Conn:          conn,
		AuthToken:     l.authToken,
		Authenticator: l.authenticator,
		Resolver:      l.resolver,
		Manager:       l.manager,
		Logger:        l.logger,
		PortAlloc:     l.portAlloc,
		Domain:        l.domain,
		TunnelDomain:  l.tunnelDomain,
		PublicPort:    l.publicPort,
		HTTPHandler:   l.httpHandler,
		GroupManager:  l.groupManager,
		HTTPListener:  l.httpListener,
		RemoteIP:      remoteIP,
	})
	tcpConn.SetAllowedTunnelTypes(l.allowedTunnelTypes)
	tcpConn.SetAllowedTransports(l.allowedTransports)
	tcpConn.SetBandwidthConfig(l.bandwidth, l.burstMultiplier)

	l.connections.Store(connID, tcpConn)
	l.connCount.Add(1)

	metrics.TotalConnections.Inc()
	metrics.ActiveConnections.Inc()

	defer func() {
		l.connections.Delete(connID)
		l.connCount.Add(-1)

		metrics.ActiveConnections.Dec()

		if !tcpConn.IsHandedOff() {
			_ = conn.Close()
		}
	}()

	if err := tcpConn.Handle(); err != nil {
		errStr := err.Error()

		if utils.IsNetworkError(errStr) {
			return
		}

		if utils.IsProtocolError(errStr) {
			l.logger.Warn("WebSocket tunnel protocol validation failed",
				zap.String("remote_addr", connID),
				zap.Error(err),
			)
		} else {
			l.logger.Error("WebSocket tunnel connection handling failed",
				zap.String("remote_addr", connID),
				zap.Error(err),
			)
		}
	}
}

// SetAllowedTransports sets the allowed transport protocols
func (l *Listener) SetAllowedTransports(transports []string) {
	l.allowedTransports = transports
}

// SetAllowedTunnelTypes sets the allowed tunnel types
func (l *Listener) SetAllowedTunnelTypes(types []string) {
	l.allowedTunnelTypes = types
}

func (l *Listener) SetBandwidth(bandwidth int64) {
	l.bandwidth = bandwidth
}

func (l *Listener) SetBurstMultiplier(multiplier float64) {
	if multiplier <= 0 {
		multiplier = 2.0
	}
	l.burstMultiplier = multiplier
}

// IsTransportAllowed checks if a transport is allowed
func (l *Listener) IsTransportAllowed(transport string) bool {
	return utils.ContainsIgnoreCase(l.allowedTransports, transport)
}
