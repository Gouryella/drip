package tcp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"drip/internal/server/metrics"
	"drip/internal/shared/netutil"
	"drip/internal/shared/pool"
	"drip/internal/shared/qos"
	"drip/internal/shared/utils"

	"go.uber.org/zap"
)

// Proxy exposes a public TCP port and forwards each incoming
// connection over a dedicated mux stream.
type Proxy struct {
	port      int
	subdomain string
	logger    *zap.Logger

	listener    net.Listener
	stopCh      chan struct{}
	once        sync.Once
	wg          sync.WaitGroup
	lifecycleMu sync.Mutex

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
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	if err := p.ctx.Err(); err != nil {
		if ln != nil {
			_ = ln.Close()
		}
		return err
	}
	addr := fmt.Sprintf("0.0.0.0:%d", p.port)

	if ln == nil {
		var err error
		ln, err = net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("failed to listen on port %d: %w", p.port, err)
		}
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
		p.lifecycleMu.Lock()
		close(p.stopCh)
		p.cancel()

		if p.listener != nil {
			_ = p.listener.Close()
		}
		p.lifecycleMu.Unlock()

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
	retryDelay := 5 * time.Millisecond

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
			if errors.Is(err, net.ErrClosed) {
				return
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			select {
			case <-p.stopCh:
				return
			default:
			}
			timer := time.NewTimer(retryDelay)
			select {
			case <-p.stopCh:
				timer.Stop()
				return
			case <-timer.C:
			}
			retryDelay = min(retryDelay*2, time.Second)
			continue
		}
		retryDelay = 5 * time.Millisecond

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
	// An unbuffered handoff lets a timed-out caller close late streams.
	resultCh := make(chan streamResult)

	ctx, cancel := context.WithTimeout(p.ctx, openStreamTimeout)
	defer cancel()

	go func() {
		s, err := p.openStream()
		select {
		case resultCh <- streamResult{s, err}:
		case <-ctx.Done():
			if s != nil {
				_ = s.Close()
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
	cancel() // The timeout is only for opening the stream.

	var limitedStream net.Conn = stream
	if p.limiter != nil && p.limiter.IsLimited() {
		if l, ok := p.limiter.(*qos.Limiter); ok {
			limitedStream = qos.NewLimitedConn(p.ctx, stream, l)
		}
	}

	var bytesIn atomic.Int64
	var bytesOut atomic.Int64
	err := netutil.PipeWithCallbacksAndBufferSize(
		p.ctx,
		conn,
		limitedStream,
		pool.SizeLarge,
		func(n int64) {
			bytesIn.Add(n)
			if p.stats != nil {
				p.stats.AddBytesIn(n)
			}
		},
		func(n int64) {
			bytesOut.Add(n)
			if p.stats != nil {
				p.stats.AddBytesOut(n)
			}
		},
	)
	p.recordTransferResult(bytesIn.Load()+bytesOut.Load(), err)
}

func (p *Proxy) recordTransferResult(bytesCopied int64, err error) {
	result := utils.ClassifyTransferError(p.ctx, err)
	metrics.ProxyTransferResults.WithLabelValues("tcp_proxy", result).Inc()

	if err == nil || p.logger == nil {
		return
	}

	fields := []zap.Field{
		zap.String("operation", "tcp_proxy"),
		zap.String("result", result),
		zap.Int64("bytes_copied", bytesCopied),
		zap.Int("port", p.port),
		zap.String("subdomain_hash", utils.HashForLog(p.subdomain)),
		zap.Error(err),
	}
	if utils.IsExpectedTransferResult(result) {
		p.logger.Debug("TCP proxy transfer ended", fields...)
		return
	}
	p.logger.Warn("TCP proxy transfer failed", fields...)
}
