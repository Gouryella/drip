package tcp

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	stdhttputil "net/http/httputil"
	"strings"
	"sync"
	"time"

	"drip/internal/shared/httputil"
	"drip/internal/shared/netutil"
	"drip/internal/shared/pool"
	"drip/internal/shared/protocol"

	"go.uber.org/zap"
)

const (
	externalForwardedProto        = "https"
	maxLocalResponseHeaderBytes   = 256 << 10
	responseHeaderWriteTimeout    = 30 * time.Second
	responseBodyWriteTimeout      = 10 * time.Second
	streamingResponseWriteTimeout = 30 * time.Second
)

func (c *PoolClient) handleStream(h *sessionHandle, stream net.Conn) {
	defer c.wg.Done()
	defer func() {
		h.active.Add(-1)
		c.stats.DecActiveConnections()
	}()
	defer stream.Close()

	switch c.tunnelType {
	case protocol.TunnelTypeHTTP, protocol.TunnelTypeHTTPS:
		c.handleHTTPStream(stream)
	default:
		c.handleTCPStream(stream)
	}
}

func (c *PoolClient) handleTCPStream(stream net.Conn) {
	localConn, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(c.ctx, "tcp", net.JoinHostPort(c.localHost, fmt.Sprintf("%d", c.localPort)))
	if err != nil {
		c.logger.Debug("Dial local failed", zap.Error(err))
		return
	}
	defer localConn.Close()

	if tcpConn, ok := localConn.(*net.TCPConn); ok {
		_ = tcpConn.SetNoDelay(true)
		_ = tcpConn.SetKeepAlive(true)
		_ = tcpConn.SetKeepAlivePeriod(30 * time.Second)
		_ = tcpConn.SetReadBuffer(256 * 1024)
		_ = tcpConn.SetWriteBuffer(256 * 1024)
	}

	_ = netutil.PipeWithCallbacksAndBufferSize(
		c.ctx,
		stream,
		localConn,
		pool.SizeLarge,
		func(n int64) { c.stats.AddBytesIn(n) },
		func(n int64) { c.stats.AddBytesOut(n) },
	)
}

func (c *PoolClient) handleHTTPStream(stream net.Conn) {
	_ = stream.SetReadDeadline(time.Now().Add(30 * time.Second))

	cc := netutil.NewCountingConn(stream,
		func(n int64) { c.stats.AddBytesIn(n) },
		func(n int64) { c.stats.AddBytesOut(n) },
	)

	br := bufio.NewReaderSize(cc, 32*1024)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	defer req.Body.Close()

	_ = stream.SetReadDeadline(time.Time{})

	if httputil.IsWebSocketUpgrade(req) {
		c.handleWebSocketUpgrade(&bufferedConn{Conn: cc, reader: br}, req)
		return
	}

	ctx, cancel := context.WithCancel(c.ctx)
	defer cancel()
	stopCopy := context.AfterFunc(ctx, func() {
		_ = stream.SetDeadline(time.Now())
		_ = stream.Close()
	})
	defer stopCopy()
	bodyDone := make(chan struct{})
	if req.ContentLength == 0 {
		close(bodyDone)
	} else {
		req.Body = &requestBodyCompletion{ReadCloser: req.Body, remaining: req.ContentLength, done: bodyDone}
	}
	stopWatchingDisconnect := watchStreamDisconnect(ctx, stream, cancel, nil, bodyDone)
	defer stopWatchingDisconnect()

	scheme := "http"
	if c.tunnelType == protocol.TunnelTypeHTTPS {
		scheme = "https"
	}

	targetAddr := net.JoinHostPort(c.localHost, fmt.Sprintf("%d", c.localPort))
	targetURL := fmt.Sprintf("%s://%s%s", scheme, targetAddr, req.URL.RequestURI())
	outReq, err := http.NewRequestWithContext(ctx, req.Method, targetURL, req.Body)
	if err != nil {
		httputil.WriteProxyError(cc, http.StatusBadGateway, "Bad Gateway")
		return
	}
	outReq.ContentLength = req.ContentLength
	outReq.Trailer = req.Trailer

	origHost := req.Host
	httputil.CopyHeaders(outReq.Header, req.Header)
	httputil.CleanHopByHopHeaders(outReq.Header)

	targetHost := c.localHost
	if c.localPort != 80 && c.localPort != 443 {
		targetHost = targetAddr
	}
	outReq.Host = targetHost
	outReq.Header.Set("Host", targetHost)
	if origHost != "" {
		outReq.Header.Set("X-Forwarded-Host", origHost)
	}
	if outReq.Header.Get("X-Forwarded-Proto") == "" {
		outReq.Header.Set("X-Forwarded-Proto", externalForwardedProto)
	}

	resp, err := c.httpClient.Do(outReq)
	if err != nil {
		c.logger.Debug("Local HTTP request failed",
			zap.String("method", req.Method),
			zap.Error(err),
		)
		httputil.WriteLocalServiceUnavailable(cc, c.localPort)
		return
	}
	defer resp.Body.Close()

	isSSE := httputil.IsEventStream(resp.Header)
	var chunkedBody io.WriteCloser
	if len(resp.Trailer) > 0 {
		keys := make([]string, 0, len(resp.Trailer))
		for key := range resp.Trailer {
			keys = append(keys, key)
		}
		resp.Header.Del("Content-Length")
		resp.Header.Set("Transfer-Encoding", "chunked")
		resp.Header.Set("Trailer", strings.Join(keys, ", "))
		chunkedBody = stdhttputil.NewChunkedWriter(cc)
	}

	headerWriteTimeout := responseHeaderWriteTimeout
	if isSSE {
		headerWriteTimeout = streamingResponseWriteTimeout
	}
	if err := stream.SetWriteDeadline(time.Now().Add(headerWriteTimeout)); err != nil {
		c.logger.Debug("Failed to set response header write deadline",
			zap.String("method", req.Method),
			zap.Bool("streaming", isSSE),
			zap.Error(err),
		)
	}
	if err := writeResponseHeader(cc, resp); err != nil {
		c.logger.Debug("Failed to write response headers to tunnel",
			zap.String("method", req.Method),
			zap.Bool("streaming", isSSE),
			zap.Error(err),
		)
		return
	}

	bufPtr := pool.GetBuffer(pool.SizeMedium)
	defer pool.PutBuffer(bufPtr)
	buf := (*bufPtr)[:pool.SizeMedium]
	var bodyWriter io.Writer = cc
	if chunkedBody != nil {
		bodyWriter = chunkedBody
	}
	for {
		nr, er := resp.Body.Read(buf)
		if nr > 0 {
			bodyWriteTimeout := responseBodyWriteTimeout
			if isSSE {
				bodyWriteTimeout = streamingResponseWriteTimeout
			}
			if err := stream.SetWriteDeadline(time.Now().Add(bodyWriteTimeout)); err != nil {
				c.logger.Debug("Failed to set response body write deadline",
					zap.String("method", req.Method),
					zap.Bool("streaming", isSSE),
					zap.Error(err),
				)
			}
			nw, ew := bodyWriter.Write(buf[:nr])
			if ew != nil {
				c.logger.Debug("Failed to write response body to tunnel",
					zap.String("method", req.Method),
					zap.Bool("streaming", isSSE),
					zap.Error(ew),
				)
				break
			}
			if nr != nw {
				c.logger.Debug("Short write while forwarding response body to tunnel",
					zap.String("method", req.Method),
					zap.Bool("streaming", isSSE),
					zap.Int("read_bytes", nr),
					zap.Int("written_bytes", nw),
				)
				break
			}
		}
		if er != nil {
			if er == io.EOF && chunkedBody != nil {
				_ = stream.SetWriteDeadline(time.Now().Add(responseBodyWriteTimeout))
				if err := chunkedBody.Close(); err != nil {
					return
				}
				if err := resp.Trailer.Write(cc); err != nil {
					return
				}
				_, _ = io.WriteString(cc, "\r\n")
			}
			break
		}
	}
}

type requestBodyCompletion struct {
	io.ReadCloser
	remaining int64
	done      chan struct{}
	once      sync.Once
}

func (b *requestBodyCompletion) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if b.remaining >= 0 {
		b.remaining -= int64(n)
	}
	if err != nil || b.remaining == 0 {
		b.once.Do(func() { close(b.done) })
	}
	return n, err
}

func (b *requestBodyCompletion) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(func() { close(b.done) })
	return err
}

func watchStreamDisconnect(ctx context.Context, stream net.Conn, cancel context.CancelFunc, body io.Closer, ready ...<-chan struct{}) func() {
	done := make(chan struct{})
	stop := make(chan struct{})
	go func() {
		defer close(done)
		if len(ready) > 0 {
			// Request-body reads own the stream until EOF. Watching earlier
			// would steal upload bytes; watching only GET leaks canceled POST SSE.
			select {
			case <-ready[0]:
			case <-ctx.Done():
				return
			case <-stop:
				return
			}
		}

		buf := make([]byte, 1)
		for {
			_, err := stream.Read(buf)
			if err != nil {
				cancel()
				if body != nil {
					_ = body.Close()
				}
				return
			}

			select {
			case <-ctx.Done():
				return
			default:
			}
		}
	}()

	return func() {
		close(stop)
		_ = stream.SetReadDeadline(time.Now())
		select {
		case <-done:
		case <-time.After(100 * time.Millisecond):
		}
		_ = stream.SetReadDeadline(time.Time{})
	}
}

func (c *PoolClient) handleWebSocketUpgrade(cc net.Conn, req *http.Request) {
	targetAddr := net.JoinHostPort(c.localHost, fmt.Sprintf("%d", c.localPort))
	localConn, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(c.ctx, "tcp", targetAddr)
	if err != nil {
		httputil.WriteProxyError(cc, http.StatusBadGateway, "WebSocket backend unavailable")
		return
	}
	defer localConn.Close()
	rawBackend := localConn
	stopBackend := context.AfterFunc(c.ctx, func() { _ = rawBackend.Close() })
	defer stopBackend()
	_ = localConn.SetDeadline(time.Now().Add(10 * time.Second))

	if c.tunnelType == protocol.TunnelTypeHTTPS {
		tlsConfig := localBackendTLSConfig(c.localHost, c.skipLocalTLSVerify)
		if c.skipLocalTLSVerify {
			c.logger.Warn("TLS certificate verification is disabled for local HTTPS WebSocket backend. " +
				"Connections to the local service are vulnerable to man-in-the-middle attacks.")
		}
		tlsConn := tls.Client(localConn, tlsConfig)
		if err := tlsConn.HandshakeContext(c.ctx); err != nil {
			httputil.WriteProxyError(cc, http.StatusBadGateway, "TLS handshake failed")
			return
		}
		localConn = tlsConn
	}

	origHost := req.Host
	req.Host = targetAddr
	if origHost != "" {
		req.Header.Set("X-Forwarded-Host", origHost)
	}
	if req.Header.Get("X-Forwarded-Proto") == "" {
		req.Header.Set("X-Forwarded-Proto", externalForwardedProto)
	}
	if err := req.Write(localConn); err != nil {
		httputil.WriteProxyError(cc, http.StatusBadGateway, "Failed to forward upgrade request")
		return
	}

	localBr := bufio.NewReader(localConn)
	resp, err := http.ReadResponse(localBr, req)
	if err != nil {
		httputil.WriteProxyError(cc, http.StatusBadGateway, "Failed to read upgrade response")
		return
	}
	defer resp.Body.Close()
	_ = cc.SetWriteDeadline(time.Now().Add(responseHeaderWriteTimeout))

	if err := resp.Write(cc); err != nil {
		return
	}

	if resp.StatusCode == http.StatusSwitchingProtocols {
		_ = cc.SetWriteDeadline(time.Time{})
		_ = localConn.SetDeadline(time.Time{})
		localRW := net.Conn(localConn)
		if localBr.Buffered() > 0 {
			localRW = &bufferedConn{Conn: localConn, reader: localBr}
		}
		_ = netutil.PipeWithCallbacksAndBufferSize(
			c.ctx,
			cc,
			localRW,
			pool.SizeLarge,
			nil, nil, // cc already counts bytes in both directions.
		)
	}
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}

var localHTTPClientWarnOnce sync.Once

func newLocalHTTPClient(tunnelType protocol.TunnelType, skipTLSVerify bool) *http.Client {
	var tlsConfig *tls.Config
	if tunnelType == protocol.TunnelTypeHTTPS {
		if skipTLSVerify {
			localHTTPClientWarnOnce.Do(func() {
				log.Println("[SECURITY WARNING] TLS certificate verification is disabled for local HTTPS backend. " +
					"Connections to the local service are vulnerable to man-in-the-middle attacks.")
			})
		}
		tlsConfig = localBackendTLSConfig("", skipTLSVerify)
	}
	return &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:           2000,
			MaxIdleConnsPerHost:    1000,
			MaxConnsPerHost:        0,
			IdleConnTimeout:        180 * time.Second,
			DisableCompression:     true,
			DisableKeepAlives:      false,
			TLSHandshakeTimeout:    5 * time.Second,
			TLSClientConfig:        tlsConfig,
			ResponseHeaderTimeout:  15 * time.Second,
			MaxResponseHeaderBytes: maxLocalResponseHeaderBytes,
			ExpectContinueTimeout:  500 * time.Millisecond,
			WriteBufferSize:        32 * 1024,
			ReadBufferSize:         32 * 1024,
			DialContext: (&net.Dialer{
				Timeout:   3 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func localBackendTLSConfig(serverName string, skipVerify bool) *tls.Config {
	if skipVerify {
		return &tls.Config{InsecureSkipVerify: true} // #nosec G402 -- only for loopback backends or explicit local TLS opt-out.
	}
	return &tls.Config{
		ServerName: serverName,
		MinVersion: tls.VersionTLS12,
	}
}

func writeResponseHeader(w io.Writer, resp *http.Response) error {
	statusLine := fmt.Sprintf("HTTP/%d.%d %d %s\r\n",
		resp.ProtoMajor, resp.ProtoMinor,
		resp.StatusCode, http.StatusText(resp.StatusCode))
	if _, err := io.WriteString(w, statusLine); err != nil {
		return err
	}
	if err := resp.Header.Write(w); err != nil {
		return err
	}
	_, err := io.WriteString(w, "\r\n")
	return err
}
