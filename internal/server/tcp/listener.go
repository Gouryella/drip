package tcp

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"drip/internal/server/metrics"
	"drip/internal/server/proxy"
	"drip/internal/server/tunnel"
	"drip/internal/shared/pool"
	"drip/internal/shared/recovery"

	"go.uber.org/zap"
	"golang.org/x/net/http2"
)

type Listener struct {
	address      string
	tlsConfig    *tls.Config
	authToken    string
	manager      *tunnel.Manager
	portAlloc    *PortAllocator
	logger       *zap.Logger
	domain       string
	tunnelDomain string
	publicPort   int
	httpHandler  http.Handler
	listener     net.Listener
	stopCh       chan struct{}
	wg           sync.WaitGroup
	connections  map[string]*Connection
	connMu       sync.RWMutex
	workerPool   *pool.WorkerPool
	recoverer    *recovery.Recoverer
	panicMetrics *recovery.PanicMetrics
	groupManager *ConnectionGroupManager
	httpServer   *http.Server
	httpListener *connQueueListener

	allowedTransports  []string
	allowedTunnelTypes []string
	bandwidth          int64
	burstMultiplier    float64
}

func NewListener(address string, tlsConfig *tls.Config, authToken string, manager *tunnel.Manager, logger *zap.Logger, portAlloc *PortAllocator, domain string, tunnelDomain string, publicPort int, httpHandler http.Handler) *Listener {
	numCPU := pool.NumCPU()
	workers := numCPU * 5
	queueSize := workers * 20
	workerPool := pool.NewWorkerPool(workers, queueSize)

	logger.Info("Worker pool configured",
		zap.Int("cpu_cores", numCPU),
		zap.Int("workers", workers),
		zap.Int("queue_size", queueSize),
	)

	panicMetrics := recovery.NewPanicMetrics(logger, nil)
	recoverer := recovery.NewRecoverer(logger, panicMetrics)

	metrics.WorkerPoolSize.Set(float64(workers))

	l := &Listener{
		address:      address,
		tlsConfig:    tlsConfig,
		authToken:    authToken,
		manager:      manager,
		portAlloc:    portAlloc,
		logger:       logger,
		domain:       domain,
		tunnelDomain: tunnelDomain,
		publicPort:   publicPort,
		httpHandler:  httpHandler,
		stopCh:       make(chan struct{}),
		connections:  make(map[string]*Connection),
		workerPool:   workerPool,
		recoverer:    recoverer,
		panicMetrics: panicMetrics,
		groupManager: NewConnectionGroupManager(logger),
	}

	if h, ok := httpHandler.(*proxy.Handler); ok {
		h.SetWSConnectionHandler(l)
		h.SetPublicPort(publicPort)
	}

	return l
}

func (l *Listener) Start() error {
	var err error

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
		MaxConcurrentStreams: 1000,
		IdleTimeout:          120 * time.Second,
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
			tcpListener.SetDeadline(time.Now().Add(1 * time.Second))
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

		l.wg.Add(1)
		submitted := l.workerPool.Submit(l.recoverer.WrapGoroutine(
			fmt.Sprintf("handleConnection-%s", conn.RemoteAddr().String()),
			func() {
				l.handleConnection(conn)
			},
		))

		if !submitted {
			l.recoverer.SafeGo(
				fmt.Sprintf("handleConnection-fallback-%s", conn.RemoteAddr().String()),
				func() {
					l.handleConnection(conn)
				},
			)
		}
	}
}

func (l *Listener) handleConnection(netConn net.Conn) {
	defer l.wg.Done()
	defer l.recoverer.RecoverWithCallback("handleConnection", func(p interface{}) {
		connID := netConn.RemoteAddr().String()
		l.connMu.Lock()
		delete(l.connections, connID)
		l.connMu.Unlock()
	})

	// Handle TLS connections
	if tlsConn, ok := netConn.(*tls.Conn); ok {
		if err := tlsConn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
			l.logger.Warn("Failed to set read deadline",
				zap.String("remote_addr", netConn.RemoteAddr().String()),
				zap.Error(err),
			)
			return
		}

		if err := tlsConn.Handshake(); err != nil {
			l.logger.Warn("TLS handshake failed",
				zap.String("remote_addr", netConn.RemoteAddr().String()),
				zap.Error(err),
			)
			return
		}

		if err := tlsConn.SetReadDeadline(time.Time{}); err != nil {
			l.logger.Warn("Failed to clear read deadline",
				zap.String("remote_addr", netConn.RemoteAddr().String()),
				zap.Error(err),
			)
			return
		}

		if tcpConn, ok := tlsConn.NetConn().(*net.TCPConn); ok {
			tcpConn.SetNoDelay(true)
			tcpConn.SetKeepAlive(true)
			tcpConn.SetKeepAlivePeriod(30 * time.Second)
			tcpConn.SetReadBuffer(256 * 1024)
			tcpConn.SetWriteBuffer(256 * 1024)
		}

		state := tlsConn.ConnectionState()
		l.logger.Info("New TLS connection",
			zap.String("remote_addr", netConn.RemoteAddr().String()),
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
			tcpConn.SetNoDelay(true)
			tcpConn.SetKeepAlive(true)
			tcpConn.SetKeepAlivePeriod(30 * time.Second)
			tcpConn.SetReadBuffer(256 * 1024)
			tcpConn.SetWriteBuffer(256 * 1024)
		}

		l.logger.Info("New plain TCP connection (reverse proxy mode)",
			zap.String("remote_addr", netConn.RemoteAddr().String()),
		)
	}

	conn := l.newConfiguredConnection(netConn)

	connID := netConn.RemoteAddr().String()
	l.trackConnection(connID, conn)
	defer l.untrackConnection(connID, conn, netConn)

	l.logConnectionError(conn.Handle(), connID, "Connection")
}

func (l *Listener) Stop() error {
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
		l.httpListener.Close()
	}

	if l.listener != nil {
		if err := l.listener.Close(); err != nil {
			l.logger.Error("Failed to close listener", zap.Error(err))
		}
	}

	l.connMu.Lock()
	for _, conn := range l.connections {
		conn.Close()
	}
	l.connMu.Unlock()

	l.wg.Wait()

	if l.workerPool != nil {
		l.workerPool.Close()
	}

	if l.groupManager != nil {
		l.groupManager.Close()
	}

	l.logger.Info("TCP listener stopped")
	return nil
}

func (l *Listener) GetActiveConnections() int {
	l.connMu.RLock()
	defer l.connMu.RUnlock()
	return len(l.connections)
}

func (l *Listener) HandleWSConnection(conn net.Conn, remoteAddr string) {
	l.wg.Add(1)
	defer l.wg.Done()

	connID := remoteAddr
	if connID == "" {
		connID = conn.RemoteAddr().String()
	}

	l.logger.Info("Handling WebSocket tunnel connection",
		zap.String("remote_addr", connID),
	)

	tcpConn := l.newConfiguredConnection(conn)

	l.trackConnection(connID, tcpConn)
	defer l.untrackConnection(connID, tcpConn, conn)

	l.logConnectionError(tcpConn.Handle(), connID, "WebSocket tunnel")
}

func (l *Listener) SetAllowedTransports(transports []string) {
	l.allowedTransports = transports
}

func (l *Listener) SetAllowedTunnelTypes(types []string) {
	l.allowedTunnelTypes = types
}

func (l *Listener) IsTransportAllowed(transport string) bool {
	return containsFold(l.allowedTransports, transport)
}

func (l *Listener) SetBurstMultiplier(multiplier float64) {
	if multiplier <= 0 {
		multiplier = 2.0
	}
	l.burstMultiplier = multiplier
}

func (l *Listener) SetBandwidth(bandwidth int64) {
	l.bandwidth = bandwidth
}

func (l *Listener) newConfiguredConnection(conn net.Conn) *Connection {
	c := NewConnection(conn, l.authToken, l.manager, l.logger, l.portAlloc, l.domain, l.tunnelDomain, l.publicPort, l.httpHandler, l.groupManager, l.httpListener)
	c.SetAllowedTunnelTypes(l.allowedTunnelTypes)
	c.SetAllowedTransports(l.allowedTransports)
	c.SetBandwidthConfig(l.bandwidth, l.burstMultiplier)
	return c
}

func (l *Listener) trackConnection(connID string, conn *Connection) {
	l.connMu.Lock()
	l.connections[connID] = conn
	l.connMu.Unlock()

	metrics.TotalConnections.Inc()
	metrics.ActiveConnections.Inc()
}

func (l *Listener) untrackConnection(connID string, conn *Connection, netConn net.Conn) {
	l.connMu.Lock()
	delete(l.connections, connID)
	l.connMu.Unlock()

	metrics.ActiveConnections.Dec()

	if !conn.IsHandedOff() {
		netConn.Close()
	}
}

// logConnectionError classifies and logs a connection handling error.
// Transient network errors are silently ignored, protocol errors are warned,
// and everything else is logged as an error.
func (l *Listener) logConnectionError(err error, connID, label string) {
	if err == nil {
		return
	}

	errStr := err.Error()

	// Transient / expected disconnects — ignore silently
	for _, substr := range []string{
		"EOF", "connection reset by peer", "broken pipe",
		"connection refused", "websocket: close",
	} {
		if strings.Contains(errStr, substr) {
			return
		}
	}

	// Protocol-level validation failures — warn
	for _, substr := range []string{
		"payload too large", "failed to read registration frame",
		"expected register frame", "failed to parse registration request",
		"failed to parse HTTP request", "tunnel type not allowed",
	} {
		if strings.Contains(errStr, substr) {
			l.logger.Warn(label+" protocol validation failed",
				zap.String("remote_addr", connID),
				zap.Error(err),
			)
			return
		}
	}

	l.logger.Error(label+" handling failed",
		zap.String("remote_addr", connID),
		zap.Error(err),
	)
}
