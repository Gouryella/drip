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

	"go.uber.org/zap"
)

// Proxy exposes a public TCP port and forwards each incoming
// connection over a dedicated mux stream.
type Proxy struct {
	port      int
	subdomain string
	logger    *zap.Logger

	listener net.Listener
	stopCh   chan struct{}
	once     sync.Once
	wg       sync.WaitGroup

	openStream func() (net.Conn, error)
	stats      trafficStats
	sem        chan struct{}

	ctx    context.Context
	cancel context.CancelFunc
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

func (p *Proxy) Start() error {
	addr := fmt.Sprintf("0.0.0.0:%d", p.port)

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to listen on port %d: %w", p.port, err)
	}
	p.listener = ln

	p.logger.Info("TCP proxy started",
		zap.Int("port", p.port),
		zap.String("subdomain", p.subdomain),
	)

	p.wg.Add(1)
	go p.acceptLoop()
	return nil
}

func (p *Proxy) Stop() {
	p.once.Do(func() {
		close(p.stopCh)
		p.cancel()

		if p.listener != nil {
			_ = p.listener.Close()
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

func (p *Proxy) acceptLoop() {
	defer p.wg.Done()

	tcpLn, _ := p.listener.(*net.TCPListener)

	for {
		select {
		case <-p.stopCh:
			return
		default:
		}

		if tcpLn != nil {
			_ = tcpLn.SetDeadline(time.Now().Add(1 * time.Second))
		}

		conn, err := p.listener.Accept()
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
		_ = tcpConn.SetReadBuffer(256 * 1024)
		_ = tcpConn.SetWriteBuffer(256 * 1024)
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

	ctx, cancel := context.WithTimeout(p.ctx, openStreamTimeout)
	defer cancel()

	go func() {
		s, err := p.openStream()
		select {
		case resultCh <- streamResult{s, err}:
		case <-ctx.Done():
			if s != nil {
				s.Close()
			}
		}
	}()

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
	case <-ctx.Done():
		p.logger.Debug("Open stream timeout")
		return
	case <-p.stopCh:
		return
	}

	defer stream.Close()

	_ = netutil.PipeWithCallbacksAndBufferSize(
		p.ctx,
		conn,
		stream,
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
