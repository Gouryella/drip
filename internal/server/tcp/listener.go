package tcp

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"drip/internal/server/metrics"
	"drip/internal/server/proxy"
	"drip/internal/server/tunnel"
	"drip/internal/shared/netutil"
	"drip/internal/shared/pool"
	"drip/internal/shared/recovery"
	"drip/internal/shared/utils"

	"go.uber.org/zap"
	"golang.org/x/net/http2"
)

type ListenerConfig struct {
	Address        string
	TLSConfig      *tls.Config
	AuthToken      string
	AllowAnonymous bool
	Manager        *tunnel.Manager
	Logger         *zap.Logger
	PortAlloc      *PortAllocator
	Domain         string
	TunnelDomain   string
	PublicPort     int
	HTTPHandler    http.Handler
	MaxDataConns   int
}

type Listener struct {
	address        string
	tlsConfig      *tls.Config
	authToken      string
	allowAnonymous bool
	manager        *tunnel.Manager
	portAlloc      *PortAllocator
	logger         *zap.Logger
	domain         string
	tunnelDomain   string
	publicPort     int
	httpHandler    http.Handler
	listener       net.Listener
	stopCh         chan struct{}
	stopOnce       sync.Once
	wg             sync.WaitGroup
	connections    sync.Map // map[string]*Connection, sync.Map for better concurrent read performance
	pendingConns   sync.Map // net.Conn -> struct{}, includes queued TLS handshakes
	admissionMu    sync.Mutex
	connCount      atomic.Int64
	connIDSeq      atomic.Int64  // unique connection ID sequence
	connSem        chan struct{} // semaphore to limit max connections
	workerPool     *pool.WorkerPool
	recoverer      *recovery.Recoverer
	panicMetrics   *recovery.PanicMetrics
	groupManager   *ConnectionGroupManager
	httpServer     *http.Server
	httpListener   *connQueueListener

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
		address:        cfg.Address,
		tlsConfig:      cfg.TLSConfig,
		authToken:      cfg.AuthToken,
		allowAnonymous: cfg.AllowAnonymous,
		manager:        cfg.Manager,
		portAlloc:      cfg.PortAlloc,
		logger:         cfg.Logger,
		domain:         cfg.Domain,
		tunnelDomain:   cfg.TunnelDomain,
		publicPort:     cfg.PublicPort,
		httpHandler:    cfg.HTTPHandler,
		stopCh:         make(chan struct{}),
		connSem:        make(chan struct{}, maxConns),
		workerPool:     workerPool,
		recoverer:      recoverer,
		panicMetrics:   panicMetrics,
		groupManager:   NewConnectionGroupManagerWithMaxDataConns(cfg.Logger, cfg.MaxDataConns),
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
		tlsConfig := l.tlsConfig.Clone()
		tlsConfig.NextProtos = append([]string{"h2", "http/1.1"}, tlsConfig.NextProtos...)
		l.listener, err = tls.Listen("tcp", l.address, tlsConfig)
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
	httpHandler := l.httpHandler
	if httpHandler == nil {
		httpHandler = http.NotFoundHandler()
	}

	l.httpServer = &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.TLS == nil {
				r.TLS, _ = r.Context().Value(httpTLSStateKey{}).(*tls.ConnectionState)
			}
			httpHandler.ServeHTTP(w, r)
		}),
		ConnContext:       httpConnectionContext,
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

type httpTLSStateKey struct{}

func httpConnectionContext(ctx context.Context, conn net.Conn) context.Context {
	if buffered, ok := conn.(*bufferedConn); ok {
		conn = buffered.Conn
	}
	if tlsConn, ok := conn.(*tls.Conn); ok {
		state := tlsConn.ConnectionState()
		return context.WithValue(ctx, httpTLSStateKey{}, &state)
	}
	return ctx
}

func (l *Listener) acceptLoop() {
	defer l.wg.Done()
	defer l.recoverer.Recover("acceptLoop")
	retryDelay := 5 * time.Millisecond

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
			if errors.Is(err, net.ErrClosed) {
				return
			}
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			select {
			case <-l.stopCh:
				return
			default:
				l.logger.Error("Failed to accept connection", zap.Error(err))
			}
			timer := time.NewTimer(retryDelay)
			select {
			case <-l.stopCh:
				timer.Stop()
				return
			case <-timer.C:
			}
			retryDelay = min(retryDelay*2, time.Second)
			continue
		}
		retryDelay = 5 * time.Millisecond

		l.admissionMu.Lock()
		select {
		case <-l.stopCh:
			l.admissionMu.Unlock()
			_ = conn.Close()
			return
		default:
		}
		// Check connection limit before adding work that Stop must wait for.
		select {
		case l.connSem <- struct{}{}:
		default:
			l.admissionMu.Unlock()
			l.logger.Warn("Connection limit reached, rejecting connection",
				zap.String("remote_addr", conn.RemoteAddr().String()),
				zap.Int64("max_conns", maxConns),
			)
			_ = conn.Close()
			continue
		}

		l.wg.Add(1)
		l.pendingConns.Store(conn, struct{}{})
		l.admissionMu.Unlock()
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
			l.pendingConns.Delete(conn)
			_ = conn.Close()
			<-l.connSem
		}
	}
}

func (l *Listener) handleConnection(netConn net.Conn) {
	serving := false
	remoteAddr := netConn.RemoteAddr().String()
	connID := fmt.Sprintf("%s#%d", remoteAddr, l.connIDSeq.Add(1))
	defer l.recoverer.Recover("handleConnection")

	cleanupRegistered := false
	defer func() {
		if !cleanupRegistered {
			_ = netConn.Close()
		}
		if !serving {
			l.pendingConns.Delete(netConn)
			<-l.connSem
			l.wg.Done()
		}
	}()

	// Handle TLS connections
	if tlsConn, ok := netConn.(*tls.Conn); ok {
		if err := tlsConn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
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

		if err := tlsConn.SetDeadline(time.Time{}); err != nil {
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
		// Preserve the concrete *tls.Conn for net/http's ALPN handling.
		// Reading the HTTP/2 preface as a Drip frame rejects valid h2 clients.
		if state.NegotiatedProtocol == "h2" || state.NegotiatedProtocol == "http/1.1" {
			if l.httpListener != nil && l.httpListener.Enqueue(tlsConn) {
				cleanupRegistered = true
			}
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

	conn := NewConnection(ConnectionConfig{
		Conn:           netConn,
		AuthToken:      l.authToken,
		AllowAnonymous: l.allowAnonymous,
		Manager:        l.manager,
		Logger:         l.logger,
		PortAlloc:      l.portAlloc,
		Domain:         l.domain,
		TunnelDomain:   l.tunnelDomain,
		PublicPort:     l.publicPort,
		HTTPHandler:    l.httpHandler,
		GroupManager:   l.groupManager,
		HTTPListener:   l.httpListener,
		RemoteIP:       netutil.ExtractIP(remoteAddr),
	})
	conn.SetAllowedTunnelTypes(l.allowedTunnelTypes)
	conn.SetAllowedTransports(l.allowedTransports)
	conn.SetBandwidthConfig(l.bandwidth, l.burstMultiplier)

	l.admissionMu.Lock()
	select {
	case <-l.stopCh:
		l.admissionMu.Unlock()
		return
	default:
	}
	l.connections.Store(connID, conn)
	l.pendingConns.Delete(netConn)
	l.connCount.Add(1)

	// Update connection metrics
	metrics.TotalConnections.Inc()
	metrics.ActiveConnections.Inc()
	l.admissionMu.Unlock()
	cleanupRegistered = true
	serving = true
	// Workers bound handshakes. Established tunnels may live for hours and
	// must not prevent other tunnels or HTTP requests from being accepted.
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		l.serveConnection(connID, netConn, conn)
	}()
	// Keep unauthenticated protocol payloads bounded by the worker count too.
	// The slot is released once registration finishes, before the long-lived
	// session starts waiting for traffic.
	select {
	case <-conn.readyCh:
	case <-finished:
	case <-l.stopCh:
	}
}

func (l *Listener) AuthenticateWebSocket(token string) bool {
	return isAuthTokenAccepted(token, l.authToken, l.allowAnonymous)
}

func (l *Listener) serveConnection(connID string, netConn net.Conn, conn *Connection) {
	defer l.wg.Done()
	defer func() { <-l.connSem }()
	defer l.recoverer.Recover("serveConnection")
	remoteAddr := netConn.RemoteAddr().String()

	defer func() {
		l.connections.Delete(connID)
		l.connCount.Add(-1)

		metrics.ActiveConnections.Dec()

		if !conn.IsHandedOff() {
			_ = netConn.Close()
		}
	}()
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

		l.admissionMu.Lock()
		close(l.stopCh)
		l.admissionMu.Unlock()
		if l.listener != nil {
			_ = l.listener.Close()
		}
		l.pendingConns.Range(func(key, value interface{}) bool {
			_ = key.(net.Conn).Close()
			return true
		})

		if l.httpServer != nil {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := l.httpServer.Shutdown(shutdownCtx); err != nil {
				l.logger.Warn("HTTP server shutdown error", zap.Error(err))
				_ = l.httpServer.Close()
			}
			l.logger.Info("HTTP server shutdown complete")
		}

		if l.httpListener != nil {
			_ = l.httpListener.Close()
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

func (l *Listener) GetActiveConnections() int {
	return int(l.connCount.Load())
}

// HandleWSConnection implements proxy.WSConnectionHandler for WebSocket tunnel connections
func (l *Listener) HandleWSConnection(conn net.Conn, remoteAddr string) {
	l.admissionMu.Lock()
	select {
	case <-l.stopCh:
		l.admissionMu.Unlock()
		_ = conn.Close()
		return
	default:
	}
	// Enforce connection limit for WebSocket connections too
	select {
	case l.connSem <- struct{}{}:
	default:
		l.admissionMu.Unlock()
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

	// Create connection handler (no TLS verification needed - already done by HTTP server)
	tcpConn := NewConnection(ConnectionConfig{
		Conn:           conn,
		AuthToken:      l.authToken,
		AllowAnonymous: l.allowAnonymous,
		Manager:        l.manager,
		Logger:         l.logger,
		PortAlloc:      l.portAlloc,
		Domain:         l.domain,
		TunnelDomain:   l.tunnelDomain,
		PublicPort:     l.publicPort,
		HTTPHandler:    l.httpHandler,
		GroupManager:   l.groupManager,
		HTTPListener:   l.httpListener,
		RemoteIP:       remoteIP,
		Transport:      "wss",
	})
	tcpConn.SetAllowedTunnelTypes(l.allowedTunnelTypes)
	tcpConn.SetAllowedTransports(l.allowedTransports)
	tcpConn.SetBandwidthConfig(l.bandwidth, l.burstMultiplier)

	l.connections.Store(connID, tcpConn)
	l.connCount.Add(1)

	metrics.TotalConnections.Inc()
	metrics.ActiveConnections.Inc()
	l.admissionMu.Unlock()

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
