package tcp

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"drip/internal/shared/httputil"
	"drip/internal/shared/netutil"
	"drip/internal/shared/pool"
	"drip/internal/shared/protocol"

	"go.uber.org/zap"
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
	localConn, err := net.DialTimeout("tcp", net.JoinHostPort(c.localHost, fmt.Sprintf("%d", c.localPort)), 10*time.Second)
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

	scheme := "http"
	if c.tunnelType == protocol.TunnelTypeHTTPS {
		scheme = "https"
	}

	targetURL := fmt.Sprintf("%s://%s:%d%s", scheme, c.localHost, c.localPort, req.URL.RequestURI())
	outReq, err := http.NewRequestWithContext(ctx, req.Method, targetURL, req.Body)
	if err != nil {
		httputil.WriteProxyError(cc, http.StatusBadGateway, "Bad Gateway")
		return
	}
	outReq.ContentLength = req.ContentLength

	origHost := req.Host
	httputil.CopyHeaders(outReq.Header, req.Header)
	httputil.CleanHopByHopHeaders(outReq.Header)

	outReq.Header.Del("Accept-Encoding")

	targetHost := c.localHost
	if c.localPort != 80 && c.localPort != 443 {
		targetHost = fmt.Sprintf("%s:%d", c.localHost, c.localPort)
	}
	outReq.Host = targetHost
	outReq.Header.Set("Host", targetHost)
	if origHost != "" {
		outReq.Header.Set("X-Forwarded-Host", origHost)
	}
	outReq.Header.Set("X-Forwarded-Proto", "https")

	resp, err := c.httpClient.Do(outReq)
	if err != nil {
		httputil.WriteProxyError(cc, http.StatusBadGateway, "Local service unavailable")
		return
	}
	defer resp.Body.Close()

	_ = stream.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if err := writeResponseHeader(cc, resp); err != nil {
		return
	}

	copyDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			stream.Close()
		case <-copyDone:
		}
	}()

	buf := make([]byte, 32*1024)
	for {
		nr, er := resp.Body.Read(buf)
		if nr > 0 {
			_ = stream.SetWriteDeadline(time.Now().Add(10 * time.Second))
			nw, ew := cc.Write(buf[:nr])
			if ew != nil || nr != nw {
				break
			}
		}
		if er != nil {
			break
		}
	}
	close(copyDone)
}

func (c *PoolClient) handleWebSocketUpgrade(cc net.Conn, req *http.Request) {
	targetAddr := net.JoinHostPort(c.localHost, fmt.Sprintf("%d", c.localPort))
	localConn, err := net.DialTimeout("tcp", targetAddr, 10*time.Second)
	if err != nil {
		httputil.WriteProxyError(cc, http.StatusBadGateway, "WebSocket backend unavailable")
		return
	}
	defer localConn.Close()

	if c.tunnelType == protocol.TunnelTypeHTTPS {
		tlsConn := tls.Client(localConn, &tls.Config{InsecureSkipVerify: true})
		if err := tlsConn.Handshake(); err != nil {
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

	if err := resp.Write(cc); err != nil {
		return
	}

	if resp.StatusCode == http.StatusSwitchingProtocols {
		localRW := net.Conn(localConn)
		if localBr.Buffered() > 0 {
			localRW = &bufferedConn{Conn: localConn, reader: localBr}
		}
		_ = netutil.PipeWithCallbacksAndBufferSize(
			c.ctx,
			cc,
			localRW,
			pool.SizeLarge,
			func(n int64) { c.stats.AddBytesIn(n) },
			func(n int64) { c.stats.AddBytesOut(n) },
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

func newLocalHTTPClient(tunnelType protocol.TunnelType) *http.Client {
	var tlsConfig *tls.Config
	if tunnelType == protocol.TunnelTypeHTTPS {
		tlsConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:          2000,
			MaxIdleConnsPerHost:   1000,
			MaxConnsPerHost:       0,
			IdleConnTimeout:       180 * time.Second,
			DisableCompression:    true,
			DisableKeepAlives:     false,
			TLSHandshakeTimeout:   5 * time.Second,
			TLSClientConfig:       tlsConfig,
			ResponseHeaderTimeout: 15 * time.Second,
			ExpectContinueTimeout: 500 * time.Millisecond,
			WriteBufferSize:       32 * 1024,
			ReadBufferSize:        32 * 1024,
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
