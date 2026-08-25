package tcp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"drip/internal/shared/netutil"
	"drip/internal/shared/pool"
	"drip/internal/shared/qos"

	"go.uber.org/zap"
)

// Proxy exposes a public TCP port and forwards each incoming
// connection over a dedicated mux stream.
type Proxy struct {
	port      int
	subdomain string
	logger    *zap.Logger

	// mu guards listener and stopped. The connection goroutine starts the proxy
	// while Stop may run concurrently from the shutdown path, so the accept
	// loop's wg.Add and the stopped flag have to be published together.
	mu       sync.Mutex
	listener net.Listener
	stopped  bool

	stopCh chan struct{}
	once   sync.Once
	wg     sync.WaitGroup

	openStream func() (net.Conn, error)
	stats      trafficStats
	sem        chan struct{}

	ctx    context.Context
	cancel context.CancelFunc

	checkIPAccess func(ip string) bool
	limiter       interface{ IsLimited() bool }
}

type trafficStats interface {
	AddBytesIn(n int64)
	AddBytesOut(n int64)
	IncActiveConnections()
	DecActiveConnections()
}

func NewProxy(ctx context.Context, port int, subdomain string, openStream func() (net.Conn, error), stats trafficStats, logger *zap.Logger) *Proxy {
	if ctx == nil {
		ctx = context.Background()
	}
	cctx, cancel := context.WithCancel(ctx)

	const maxConcurrentConnections = 10000
	var sem chan struct{}
	if maxConcurrentConnections > 0 {
		sem = make(chan struct{}, maxConcurrentConnections)
	}

	return &Proxy{
		port:       port,
		subdomain:  subdomain,
		logger:     logger,
		stopCh:     make(chan struct{}),
		openStream: openStream,
		stats:      stats,
		sem:        sem,
		ctx:        cctx,
		cancel:     cancel,
	}
}

// SetIPAccessCheck sets the IP access control check function.
func (p *Proxy) SetIPAccessCheck(check func(ip string) bool) {
	p.checkIPAccess = check
}

// SetLimiter sets the bandwidth limiter for this proxy.
func (p *Proxy) SetLimiter(limiter interface{ IsLimited() bool }) {
	p.limiter = limiter
}

func (p *Proxy) Start() error {
	return p.StartWithListener(nil)
}

func (p *Proxy) StartWithListener(ln net.Listener) error {
	if ln == nil {
		addr := fmt.Sprintf("0.0.0.0:%d", p.port)
		var err error
		ln, err = net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("failed to listen on port %d: %w", p.port, err)
		}
	}
	// Publish the listener and register the accept loop with the wait group
	// under one lock. Stop sets stopped under the same lock before it waits, so
	// either this Add is visible to that Wait, or we observe stopped and never
	// start at all. Without this a proxy stopped mid-startup would leave its
	// accept loop running after Stop returned.
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		_ = ln.Close()
		return fmt.Errorf("tcp proxy for port %d is already stopped", p.port)
	}
	p.listener = ln
	p.wg.Add(1)
	p.mu.Unlock()

	p.logger.Info("TCP proxy started",
		zap.Int("port", p.port),
		zap.String("subdomain", p.subdomain),
	)

	go p.acceptLoop(ln)
	return nil
}

func (p *Proxy) Stop() {
	p.once.Do(func() {
		close(p.stopCh)
		p.cancel()

		p.mu.Lock()
		p.stopped = true
		ln := p.listener
		p.mu.Unlock()

		if ln != nil {
			_ = ln.Close()
		}

		done := make(chan struct{})
		go func() {
			p.wg.Wait()
			close(done)
		}()

		const stopTimeout = 30 * time.Second

		select {
		case <-done:
			p.logger.Info("TCP proxy stopped",
				zap.Int("port", p.port),
				zap.String("subdomain", p.subdomain),
			)
		case <-time.After(stopTimeout):
			p.logger.Warn("TCP proxy stop timed out",
				zap.Int("port", p.port),
				zap.String("subdomain", p.subdomain),
				zap.Duration("timeout", stopTimeout),
			)
		}
	})
}

func (p *Proxy) acceptLoop(ln net.Listener) {
	defer p.wg.Done()

	tcpLn, _ := ln.(*net.TCPListener)

	for {
		select {
		case <-p.stopCh:
			return
		default:
		}

		if tcpLn != nil {
			_ = tcpLn.SetDeadline(time.Now().Add(1 * time.Second))
		}

		conn, err := ln.Accept()
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			select {
			case <-p.stopCh:
				return
			default:
				continue
			}
		}

		p.wg.Add(1)
		go p.handleConn(conn)
	}
}

func (p *Proxy) handleConn(conn net.Conn) {
	defer p.wg.Done()
	defer conn.Close()

	if p.checkIPAccess != nil {
		clientIP := netutil.ExtractIP(conn.RemoteAddr().String())
		if !p.checkIPAccess(clientIP) {
			p.logger.Debug("IP access denied",
				zap.String("ip", clientIP),
				zap.Int("port", p.port),
			)
			return
		}
	}

	if p.sem != nil {
		select {
		case p.sem <- struct{}{}:
			defer func() { <-p.sem }()
		default:
			return
		}
	}

	if p.stats != nil {
		p.stats.IncActiveConnections()
		defer p.stats.DecActiveConnections()
	}

	if tcpConn, ok := conn.(*net.TCPConn); ok {
		_ = tcpConn.SetNoDelay(true)
		_ = tcpConn.SetKeepAlive(true)
		_ = tcpConn.SetKeepAlivePeriod(30 * time.Second)
		_ = tcpConn.SetReadBuffer(512 * 1024)
		_ = tcpConn.SetWriteBuffer(512 * 1024)
	}

	if p.openStream == nil {
		return
	}

	const openStreamTimeout = 3 * time.Second
	type streamResult struct {
		stream net.Conn
		err    error
	}
	resultCh := make(chan streamResult, 1)

	go func() {
		s, err := p.openStream()
		resultCh <- streamResult{s, err}
	}()

	drainLateResult := func() {
		go func() {
			result := <-resultCh
			if result.stream != nil {
				_ = result.stream.Close()
			}
		}()
	}

	var stream net.Conn
	select {
	case result := <-resultCh:
		if result.err != nil {
			if !errors.Is(result.err, net.ErrClosed) {
				p.logger.Debug("Open stream failed", zap.Error(result.err))
			}
			return
		}
		stream = result.stream
	case <-time.After(openStreamTimeout):
		drainLateResult()
		p.logger.Debug("Open stream timeout")
		return
	case <-p.stopCh:
		drainLateResult()
		return
	case <-p.ctx.Done():
		drainLateResult()
		return
	}

	defer stream.Close()

	var limitedStream net.Conn = stream
	if p.limiter != nil && p.limiter.IsLimited() {
		if l, ok := p.limiter.(*qos.Limiter); ok {
			limitedStream = qos.NewLimitedConn(p.ctx, stream, l)
		}
	}

	_ = netutil.PipeWithCallbacksAndBufferSize(
		p.ctx,
		conn,
		limitedStream,
		pool.SizeLarge,
		func(n int64) {
			if p.stats != nil {
				p.stats.AddBytesIn(n)
			}
		},
		func(n int64) {
			if p.stats != nil {
				p.stats.AddBytesOut(n)
			}
		},
	)
}
