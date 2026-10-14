package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"

	"drip/internal/server/tunnel"
	"drip/internal/shared/httputil"
	"drip/internal/shared/netutil"
	"drip/internal/shared/pool"
	"drip/internal/shared/protocol"
	"drip/internal/shared/qos"
	"drip/internal/shared/utils"
	"drip/pkg/config"
)

// bufio.Reader pool to reduce allocations on hot path
var bufioReaderPool = sync.Pool{
	New: func() interface{} {
		return bufio.NewReaderSize(nil, 32*1024)
	},
}

// bufioWriter pool for HTTP request forwarding
var bufioWriterPool = sync.Pool{
	New: func() interface{} {
		return bufio.NewWriterSize(nil, 32*1024)
	},
}

const (
	openStreamTimeout               = 3 * time.Second
	defaultMaxResponseHeaderBytes   = 256 << 10
	defaultResponseHeaderTimeout    = 15 * time.Second
	defaultStreamingWriteTimeout    = 30 * time.Second
	defaultForwardWriteTimeout      = 30 * time.Second
	defaultForwardFlushWriteTimeout = 10 * time.Second
)

var errResponseHeadersTooLarge = errors.New("tunnel response headers exceeded limit")

type HandlerConfig struct {
	Manager                    *tunnel.Manager
	Logger                     *zap.Logger
	ServerDomain               string
	TunnelDomain               string
	AuthToken                  string
	MetricsToken               string
	TrustedProxies             []string
	MaxRequestBodyBytes        int64
	UnsafeUnlimitedRequestBody bool
	MaxResponseHeaderBytes     int64
	ResponseHeaderTimeout      time.Duration
	StreamingWriteTimeout      time.Duration
}

type Handler struct {
	manager                *tunnel.Manager
	logger                 *zap.Logger
	serverDomain           string
	tunnelDomain           string
	authToken              string
	metricsToken           string
	trustedProxies         *netutil.TrustedProxySet
	publicPort             int
	maxRequestBodyBytes    int64
	maxResponseHeaderBytes int64
	responseHeaderTimeout  time.Duration
	streamingWriteTimeout  time.Duration

	// WebSocket tunnel support
	wsUpgrader    websocket.Upgrader
	wsConnHandler WSConnectionHandler

	// Server capabilities
	allowedTransports  []string
	allowedTunnelTypes []string
}

// WSConnectionHandler handles WebSocket tunnel connections
type WSConnectionHandler interface {
	HandleWSConnection(conn net.Conn, remoteAddr string)
}

func NewHandler(cfg HandlerConfig) *Handler {
	serverDomain := cfg.ServerDomain
	tunnelDomain := cfg.TunnelDomain
	logger := cfg.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	trustedProxies, err := netutil.NewTrustedProxySet(cfg.TrustedProxies)
	if err != nil {
		logger.Warn("Ignoring invalid trusted proxy configuration", zap.Error(err))
		trustedProxies = &netutil.TrustedProxySet{}
	}
	maxRequestBodyBytes := cfg.MaxRequestBodyBytes
	if maxRequestBodyBytes == 0 && !cfg.UnsafeUnlimitedRequestBody {
		maxRequestBodyBytes = config.DefaultMaxRequestBodyBytes
	}
	if maxRequestBodyBytes < 0 {
		maxRequestBodyBytes = config.DefaultMaxRequestBodyBytes
	}
	maxResponseHeaderBytes := cfg.MaxResponseHeaderBytes
	if maxResponseHeaderBytes <= 0 {
		maxResponseHeaderBytes = defaultMaxResponseHeaderBytes
	}
	responseHeaderTimeout := cfg.ResponseHeaderTimeout
	if responseHeaderTimeout <= 0 {
		responseHeaderTimeout = defaultResponseHeaderTimeout
	}
	streamingWriteTimeout := cfg.StreamingWriteTimeout
	if streamingWriteTimeout <= 0 {
		streamingWriteTimeout = defaultStreamingWriteTimeout
	}
	return &Handler{
		manager:                cfg.Manager,
		logger:                 logger,
		serverDomain:           serverDomain,
		tunnelDomain:           tunnelDomain,
		authToken:              cfg.AuthToken,
		metricsToken:           cfg.MetricsToken,
		trustedProxies:         trustedProxies,
		maxRequestBodyBytes:    maxRequestBodyBytes,
		maxResponseHeaderBytes: maxResponseHeaderBytes,
		responseHeaderTimeout:  responseHeaderTimeout,
		streamingWriteTimeout:  streamingWriteTimeout,
		wsUpgrader: websocket.Upgrader{
			ReadBufferSize:  256 * 1024,
			WriteBufferSize: 256 * 1024,
			CheckOrigin: func(r *http.Request) bool {
				origin := r.Header.Get("Origin")
				if origin == "" {
					return true // Non-browser clients may not send Origin
				}
				originURL, err := url.Parse(origin)
				if err != nil {
					return false
				}
				originHost := originURL.Host
				if originHost == "" {
					originHost = originURL.Path
				}
				// Allow requests from server domain or tunnel domain
				if originHost == serverDomain || originHost == tunnelDomain {
					return true
				}
				// Allow subdomains of tunnel domain
				if tunnelDomain != "" && strings.HasSuffix(originHost, "."+tunnelDomain) {
					return true
				}
				return false
			},
		},
	}
}

// SetWSConnectionHandler sets the handler for WebSocket tunnel connections
func (h *Handler) SetWSConnectionHandler(handler WSConnectionHandler) {
	h.wsConnHandler = handler
}

// SetPublicPort sets the public port for URL generation
func (h *Handler) SetPublicPort(port int) {
	h.publicPort = port
}

// SetAllowedTransports sets the allowed transport protocols
func (h *Handler) SetAllowedTransports(transports []string) {
	h.allowedTransports = transports
}

// SetAllowedTunnelTypes sets the allowed tunnel types
func (h *Handler) SetAllowedTunnelTypes(types []string) {
	h.allowedTunnelTypes = types
}

// IsTransportAllowed checks if a transport is allowed
func (h *Handler) IsTransportAllowed(transport string) bool {
	return utils.ContainsIgnoreCase(h.allowedTransports, transport)
}

// IsTunnelTypeAllowed checks if a tunnel type is allowed
func (h *Handler) IsTunnelTypeAllowed(tunnelType string) bool {
	return utils.ContainsIgnoreCase(h.allowedTunnelTypes, tunnelType)
}

// GetPreferredTransport returns the preferred transport for auto-detection
func (h *Handler) GetPreferredTransport() string {
	if len(h.allowedTransports) == 0 {
		return "tcp"
	}
	if len(h.allowedTransports) == 1 {
		return h.allowedTransports[0]
	}
	return "tcp"
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Discovery endpoint for client auto-detection
	if r.URL.Path == "/_drip/discover" {
		h.serveDiscovery(w, r)
		return
	}

	// WebSocket tunnel endpoint - must be checked before other routes
	if r.URL.Path == "/_drip/ws" {
		h.handleTunnelWebSocket(w, r)
		return
	}

	if r.URL.Path == "/health" {
		h.serveHealth(w, r)
		return
	}

	if h.isManagementPath(r.URL.Path) && h.isManagementHost(r.Host) {
		h.serveManagementPath(w, r)
		return
	}

	subdomain, result := h.extractSubdomain(r.Host)
	switch result {
	case subdomainHome:
		h.serveHomePage(w, r)
		return
	case subdomainNotFound:
		h.serveTunnelNotFound(w, r)
		return
	}

	tconn, ok := h.manager.Get(subdomain)
	if !ok || tconn == nil {
		h.serveTunnelNotFound(w, r)
		return
	}
	if tconn.IsClosed() {
		http.Error(w, "Tunnel connection closed", http.StatusBadGateway)
		return
	}

	if tconn.HasIPAccessControl() {
		clientIP := netutil.ExtractClientIPWithTrustedProxies(r, h.trustedProxies)
		if !tconn.IsIPAllowed(clientIP) {
			http.Error(w, "Access denied: your IP is not allowed", http.StatusForbidden)
			return
		}
	}

	if auth := tconn.GetProxyAuth(); auth != nil && auth.Enabled {
		clientIP := netutil.ExtractClientIPWithTrustedProxies(r, h.trustedProxies)
		authType := proxyAuthType(auth)
		rateLimitKey := authRateLimitKey(clientIP, subdomain)

		if authLimiter.isRateLimited(rateLimitKey) {
			h.recordProxyAuthLockout(r, authType, subdomain, clientIP)
			w.Header().Set("Retry-After", "60")
			http.Error(w, "Too many failed authentication attempts. Please try again later.", http.StatusTooManyRequests)
			return
		}

		if isBearerProxyAuth(auth) {
			if ok, reason := h.checkBearerAuthenticated(r, auth); !ok {
				authLimiter.recordFailure(rateLimitKey)
				h.recordProxyAuthFailure(r, authType, reason, subdomain, clientIP)
				h.serveBearerAuthRequired(w, "drip")
				return
			}
			authLimiter.resetFailures(rateLimitKey)
		} else {
			if r.URL.Path == "/_drip/login" {
				h.handleProxyLoginWithRateLimit(w, r, tconn, subdomain, clientIP)
				return
			}
			if !h.isProxyAuthenticated(r, subdomain, tconn) {
				h.serveLoginPage(w, r, subdomain, "")
				return
			}
		}
	}

	tType := tconn.GetTunnelType()
	if tType != "" && tType != protocol.TunnelTypeHTTP && tType != protocol.TunnelTypeHTTPS {
		http.Error(w, "Tunnel does not accept HTTP traffic", http.StatusBadGateway)
		return
	}

	if r.Method == http.MethodConnect {
		http.Error(w, "CONNECT not supported for HTTP tunnels", http.StatusMethodNotAllowed)
		return
	}
	r = h.forwardedRequest(r)

	if h.isWebSocketUpgrade(r) {
		h.handleWebSocket(w, r, tconn)
		return
	}

	if h.maxRequestBodyBytes > 0 && r.Body != nil {
		if r.ContentLength > h.maxRequestBodyBytes {
			h.rejectRequestBodyTooLarge(w, r, subdomain)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, h.maxRequestBodyBytes)
	}

	stream, err := h.openStreamWithTimeout(tconn)
	if err != nil {
		httputil.SetCloseConnection(w)
		http.Error(w, "Tunnel unavailable", http.StatusBadGateway)
		return
	}
	defer stream.Close()
	stop := context.AfterFunc(r.Context(), func() {
		_ = stream.SetDeadline(time.Now())
		_ = stream.Close()
	})
	defer stop()

	tconn.IncActiveConnections()
	defer tconn.DecActiveConnections()

	var limitedStream net.Conn = stream
	if limiter := tconn.GetLimiter(); limiter != nil && limiter.IsLimited() {
		if l, ok := limiter.(*qos.Limiter); ok {
			limitedStream = qos.NewLimitedConn(r.Context(), stream, l)
		}
	}

	countingStream := netutil.NewCountingConn(limitedStream,
		tconn.AddBytesOut,
		tconn.AddBytesIn,
	)

	// Use pooled bufio.Writer to batch small writes and reduce syscalls
	bw := bufioWriterPool.Get().(*bufio.Writer)
	bw.Reset(countingStream)
	if err := countingStream.SetWriteDeadline(time.Now().Add(defaultForwardWriteTimeout)); err != nil {
		h.logger.Warn("Failed to set tunnel request write deadline",
			zap.String("subdomain", subdomain),
			zap.String("method", r.Method),
			zap.Error(err),
		)
	}
	if err := r.Write(bw); err != nil {
		bw.Reset(nil)
		bufioWriterPool.Put(bw)
		httputil.SetCloseConnection(w)
		_ = r.Body.Close()
		if isRequestBodyLimitError(err) {
			h.rejectRequestBodyTooLarge(w, r, subdomain)
		} else {
			h.logger.Warn("Failed to forward tunneled request",
				zap.String("subdomain", subdomain),
				zap.String("method", r.Method),
				zap.Error(err),
			)
			http.Error(w, "Forward failed", http.StatusBadGateway)
		}
		return
	}
	if err := countingStream.SetWriteDeadline(time.Now().Add(defaultForwardFlushWriteTimeout)); err != nil {
		h.logger.Warn("Failed to refresh tunnel request write deadline",
			zap.String("subdomain", subdomain),
			zap.String("method", r.Method),
			zap.Error(err),
		)
	}
	if err := bw.Flush(); err != nil {
		bw.Reset(nil)
		bufioWriterPool.Put(bw)
		httputil.SetCloseConnection(w)
		_ = r.Body.Close()
		h.logger.Warn("Failed to flush tunneled request",
			zap.String("subdomain", subdomain),
			zap.String("method", r.Method),
			zap.Error(err),
		)
		http.Error(w, "Forward flush failed", http.StatusBadGateway)
		return
	}
	if err := countingStream.SetWriteDeadline(time.Time{}); err != nil {
		h.logger.Warn("Failed to clear tunnel request write deadline",
			zap.String("subdomain", subdomain),
			zap.String("method", r.Method),
			zap.Error(err),
		)
	}
	bw.Reset(nil)
	bufioWriterPool.Put(bw)

	reader := bufioReaderPool.Get().(*bufio.Reader)
	resp, err := h.readTunnelResponse(reader, countingStream, r)
	if err != nil {
		reader.Reset(nil)
		bufioReaderPool.Put(reader)
		httputil.SetCloseConnection(w)
		h.respondTunnelReadError(w, r, subdomain, err)
		return
	}
	defer func() {
		_ = resp.Body.Close()
		reader.Reset(nil)
		bufioReaderPool.Put(reader)
	}()

	h.copyResponseHeaders(w.Header(), resp.Header, r.Host)
	for key := range resp.Trailer {
		w.Header().Add("Trailer", key)
	}

	statusCode := resp.StatusCode
	if statusCode == 0 {
		statusCode = http.StatusOK
	}

	if r.Method == http.MethodHead || statusCode == http.StatusNoContent || statusCode == http.StatusNotModified {
		if resp.ContentLength >= 0 {
			httputil.SetContentLength(w, resp.ContentLength)
		} else {
			w.Header().Del("Content-Length")
		}
		w.WriteHeader(statusCode)
		return
	}

	streamingResponse := httputil.IsEventStream(resp.Header)
	if streamingResponse {
		w.Header().Del("Content-Length")
	} else if resp.ContentLength >= 0 {
		httputil.SetContentLength(w, resp.ContentLength)
	} else {
		w.Header().Del("Content-Length")
	}

	w.WriteHeader(statusCode)

	if streamingResponse {
		streamWriter := newStreamingResponseWriter(w, h.streamingWriteTimeout)
		if err := streamWriter.Flush(); err != nil {
			h.logger.Debug("Failed to flush streaming response headers",
				zap.String("subdomain", subdomain),
				zap.String("method", r.Method),
				zap.Error(err),
			)
			return
		}
		n, err := copyResponseBodyFlushing(streamWriter, resp.Body)
		h.recordProxyTransferResult(r.Context(), proxyTransferHTTPStreamingResponse, n, err)
		if err == nil {
			httputil.CopyHeaders(w.Header(), resp.Trailer)
		}
		if err != nil {
			h.logger.Debug("Streaming response copy stopped",
				zap.String("subdomain", subdomain),
				zap.String("method", r.Method),
				zap.Error(err),
			)
		}
		return
	}

	// Use pooled buffer for zero-copy optimization
	buf := pool.GetBuffer(pool.SizeLarge)
	defer pool.PutBuffer(buf)

	n, err := io.CopyBuffer(w, resp.Body, (*buf)[:])
	h.recordProxyTransferResult(r.Context(), proxyTransferHTTPResponse, n, err)
	if err != nil {
		// A failed close-delimited/chunked response must not look complete.
		panic(http.ErrAbortHandler)
	}
	httputil.CopyHeaders(w.Header(), resp.Trailer)
}

// forwardedRequest supplies metadata derived from the peer and configured
// trusted proxies, so a public caller cannot forge the backend's client IP.
func (h *Handler) forwardedRequest(r *http.Request) *http.Request {
	out := r.Clone(r.Context())
	out.Trailer = r.Trailer // Body reads populate this map as trailers arrive.
	if len(out.Trailer) > 0 {
		out.ContentLength = -1
		out.TransferEncoding = []string{"chunked"}
	}
	isUpgrade := httputil.IsWebSocketUpgrade(r)
	httputil.CleanHopByHopHeaders(out.Header)
	if isUpgrade {
		out.Header.Set("Connection", "Upgrade")
		out.Header.Set("Upgrade", "websocket")
	}
	out.Header.Del("Forwarded")
	clientIP := netutil.ExtractClientIPWithTrustedProxies(r, h.trustedProxies)
	if net.ParseIP(clientIP) != nil {
		out.Header.Set("X-Forwarded-For", clientIP)
		out.Header.Set("X-Real-IP", clientIP)
	} else {
		out.Header.Del("X-Forwarded-For")
		out.Header.Del("X-Real-IP")
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	} else if h.trustedProxies.Contains(netutil.ExtractRemoteIP(r.RemoteAddr)) {
		if proto := r.Header.Get("X-Forwarded-Proto"); proto == "http" || proto == "https" {
			scheme = proto
		}
	}
	out.Header.Set("X-Forwarded-Proto", scheme)
	out.Header.Set("X-Forwarded-Host", r.Host)
	return out
}

func (h *Handler) rejectRequestBodyTooLarge(w http.ResponseWriter, r *http.Request, subdomain string) {
	httputil.SetCloseConnection(w)
	h.logger.Warn("Rejected over-limit tunneled request body",
		zap.String("subdomain", subdomain),
		zap.String("method", r.Method),
		zap.Int64("content_length", r.ContentLength),
		zap.Int64("max_request_body_bytes", h.maxRequestBodyBytes),
	)
	http.Error(w, "Request body too large", http.StatusRequestEntityTooLarge)
}

func isRequestBodyLimitError(err error) bool {
	var maxBytesErr *http.MaxBytesError
	return errors.As(err, &maxBytesErr) || strings.Contains(err.Error(), "http: request body too large")
}

func (h *Handler) readTunnelResponse(reader *bufio.Reader, stream net.Conn, req *http.Request) (*http.Response, error) {
	limitedReader := &responseHeaderLimitReader{
		reader:    stream,
		remaining: h.maxResponseHeaderBytes,
	}
	reader.Reset(limitedReader)

	if err := stream.SetReadDeadline(time.Now().Add(h.responseHeaderTimeout)); err != nil {
		h.logger.Warn("Failed to set tunnel response header read deadline",
			zap.String("method", req.Method),
			zap.Error(err),
		)
	}

	resp, err := http.ReadResponse(reader, req)
	if clearErr := stream.SetReadDeadline(time.Time{}); clearErr != nil {
		h.logger.Warn("Failed to clear tunnel response header read deadline",
			zap.String("method", req.Method),
			zap.Error(clearErr),
		)
	}
	if err != nil {
		return nil, err
	}

	limitedReader.allowBody()
	return resp, nil
}

func (h *Handler) respondTunnelReadError(w http.ResponseWriter, r *http.Request, subdomain string, err error) {
	fields := []zap.Field{
		zap.String("subdomain", subdomain),
		zap.String("method", r.Method),
		zap.Int64("max_response_header_bytes", h.maxResponseHeaderBytes),
		zap.Error(err),
	}

	if errors.Is(err, errResponseHeadersTooLarge) {
		h.logger.Warn("Rejected over-limit tunnel response headers", fields...)
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		h.logger.Warn("Timed out reading tunnel response headers", fields...)
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return
	}

	h.logger.Warn("Failed to read tunnel response", fields...)
	http.Error(w, "Read response failed", http.StatusBadGateway)
}

type responseHeaderLimitReader struct {
	reader    io.Reader
	remaining int64
	unlimited bool
}

func (r *responseHeaderLimitReader) Read(p []byte) (int, error) {
	if r.unlimited {
		return r.reader.Read(p)
	}
	if r.remaining <= 0 {
		return 0, errResponseHeadersTooLarge
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.reader.Read(p)
	r.remaining -= int64(n)
	return n, err
}

func (r *responseHeaderLimitReader) allowBody() {
	r.unlimited = true
}

type streamResult struct {
	stream net.Conn
	err    error
}

func (h *Handler) openStreamWithTimeout(tconn *tunnel.Connection) (net.Conn, error) {
	return h.openStream(tconn, openStreamTimeout)
}

func (h *Handler) openStream(tconn *tunnel.Connection, timeout time.Duration) (net.Conn, error) {
	// Buffered so OpenStream can always complete without blocking; the timeout
	// path asynchronously drains and closes any late stream.
	ch := make(chan streamResult, 1)

	go func() {
		s, err := tconn.OpenStream()
		ch <- streamResult{s, err}
	}()

	select {
	case r := <-ch:
		return r.stream, r.err
	case <-time.After(timeout):
		go func() {
			r := <-ch
			if r.stream != nil {
				_ = r.stream.Close()
			}
		}()
		return nil, fmt.Errorf("open stream timeout")
	}
}

func (h *Handler) copyResponseHeaders(dst http.Header, src http.Header, proxyHost string) {
	httputil.CleanHopByHopHeaders(src)
	for key, values := range src {
		canonicalKey := http.CanonicalHeaderKey(key)

		// Hop-by-hop headers must not be forwarded.
		if canonicalKey == "Connection" ||
			canonicalKey == "Keep-Alive" ||
			canonicalKey == "Transfer-Encoding" ||
			canonicalKey == "Upgrade" ||
			canonicalKey == "Proxy-Connection" ||
			canonicalKey == "Te" ||
			canonicalKey == "Trailer" {
			continue
		}

		if canonicalKey == "Location" && len(values) > 0 {
			dst.Set("Location", h.rewriteLocationHeader(values[0], proxyHost))
			continue
		}

		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func (h *Handler) rewriteLocationHeader(location, proxyHost string) string {
	if !strings.HasPrefix(location, "http://") && !strings.HasPrefix(location, "https://") {
		return location
	}

	locationURL, err := url.Parse(location)
	if err != nil {
		return location
	}

	host := locationURL.Hostname()
	ip := net.ParseIP(host)
	if strings.EqualFold(host, "localhost") || (ip != nil && ip.IsLoopback()) {
		locationURL.Scheme = "https"
		locationURL.Host = proxyHost
		return locationURL.String()
	}

	return location
}

type subdomainResult int

const (
	subdomainHome subdomainResult = iota
	subdomainFound
	subdomainNotFound
)

func (h *Handler) extractSubdomain(host string) (string, subdomainResult) {
	host = normalizeRequestHost(host)
	serverDomain := normalizeRequestHost(h.serverDomain)
	tunnelDomain := normalizeRequestHost(h.tunnelDomain)

	if host == serverDomain {
		return "", subdomainHome
	}

	suffix := "." + tunnelDomain
	if strings.HasSuffix(host, suffix) {
		return strings.TrimSuffix(host, suffix), subdomainFound
	}

	if host == tunnelDomain {
		return "", subdomainNotFound
	}

	return "", subdomainNotFound
}

func (h *Handler) isManagementPath(path string) bool {
	return path == "/stats" ||
		path == "/metrics" ||
		path == "/admin" ||
		strings.HasPrefix(path, "/admin/")
}

func (h *Handler) isManagementHost(host string) bool {
	host = normalizeRequestHost(host)
	serverDomain := normalizeRequestHost(h.serverDomain)
	if host == "" || serverDomain == "" {
		return false
	}
	return host == serverDomain || host == "admin."+serverDomain
}

func (h *Handler) serveManagementPath(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/stats":
		h.serveStats(w, r)
	case "/metrics":
		h.serveMetrics(w, r)
	default:
		http.NotFound(w, r)
	}
}

func normalizeRequestHost(host string) string {
	host = strings.TrimSpace(strings.ToLower(host))
	if host == "" {
		return ""
	}

	if parsedHost, _, err := net.SplitHostPort(host); err == nil {
		host = parsedHost
	} else if strings.Count(host, ":") == 1 {
		if idx := strings.LastIndex(host, ":"); idx != -1 {
			host = host[:idx]
		}
	}

	host = strings.TrimPrefix(host, "[")
	host = strings.TrimSuffix(host, "]")
	host = strings.TrimSuffix(host, ".")
	return host
}

func (h *Handler) validateMetricsAuth(w http.ResponseWriter, r *http.Request, realm string) bool {
	expectedToken := h.metricsAuthToken()
	if expectedToken == "" {
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s"`, realm))
		http.Error(w, "Unauthorized: configure a metrics token or server auth token", http.StatusUnauthorized)
		return false
	}

	token := extractBearerToken(r.Header.Get("Authorization"))

	if !utils.ConstantTimeEqualString(token, expectedToken) {
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s"`, realm))
		http.Error(w, "Unauthorized: provide metrics token via 'Authorization: Bearer <token>' header", http.StatusUnauthorized)
		return false
	}

	return true
}

func (h *Handler) metricsAuthToken() string {
	if h.metricsToken != "" {
		return h.metricsToken
	}
	return h.authToken
}
