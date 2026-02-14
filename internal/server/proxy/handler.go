package proxy

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	json "github.com/goccy/go-json"
	"github.com/gorilla/websocket"

	"drip/internal/server/tunnel"
	"drip/internal/shared/httputil"
	"drip/internal/shared/netutil"
	"drip/internal/shared/pool"
	"drip/internal/shared/protocol"
	"drip/internal/shared/qos"
	"drip/internal/shared/wsutil"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"
)

// bufio.Reader pool to reduce allocations on hot path
var bufioReaderPool = sync.Pool{
	New: func() interface{} {
		return bufio.NewReaderSize(nil, 32*1024)
	},
}

const openStreamTimeout = 3 * time.Second
const authCookieName = "drip_auth"
const authSessionDuration = 24 * time.Hour

type authSession struct {
	subdomain string
	expiresAt time.Time
}

type authSessionStore struct {
	mu       sync.RWMutex
	sessions map[string]*authSession
}

var sessionStore = &authSessionStore{
	sessions: make(map[string]*authSession),
}

func (s *authSessionStore) create(subdomain string) string {
	token := generateSessionToken()
	s.mu.Lock()
	s.sessions[token] = &authSession{
		subdomain: subdomain,
		expiresAt: time.Now().Add(authSessionDuration),
	}
	s.mu.Unlock()
	return token
}

func (s *authSessionStore) validate(token, subdomain string) bool {
	s.mu.RLock()
	session, ok := s.sessions[token]
	s.mu.RUnlock()

	if !ok {
		return false
	}
	if time.Now().After(session.expiresAt) {
		s.mu.Lock()
		delete(s.sessions, token)
		s.mu.Unlock()
		return false
	}
	return session.subdomain == subdomain
}

func generateSessionToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	hash := sha256.Sum256(b)
	return hex.EncodeToString(hash[:])
}

type Handler struct {
	manager      *tunnel.Manager
	logger       *zap.Logger
	serverDomain string
	tunnelDomain string
	authToken    string
	metricsToken string
	publicPort   int

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

var privateNetworks []*net.IPNet

func init() {
	privateCIDRs := []string{
		"127.0.0.0/8",
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"::1/128",
		"fc00::/7",
		"fe80::/10",
	}
	for _, cidr := range privateCIDRs {
		_, ipNet, _ := net.ParseCIDR(cidr)
		privateNetworks = append(privateNetworks, ipNet)
	}
}

func NewHandler(manager *tunnel.Manager, logger *zap.Logger, serverDomain, tunnelDomain string, authToken string, metricsToken string) *Handler {
	return &Handler{
		manager:      manager,
		logger:       logger,
		serverDomain: serverDomain,
		tunnelDomain: tunnelDomain,
		authToken:    authToken,
		metricsToken: metricsToken,
		wsUpgrader: websocket.Upgrader{
			ReadBufferSize:  256 * 1024,
			WriteBufferSize: 256 * 1024,
			CheckOrigin: func(r *http.Request) bool {
				return true // Allow all origins for tunnel connections
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
	return containsFold(h.allowedTransports, transport)
}

// IsTunnelTypeAllowed checks if a tunnel type is allowed
func (h *Handler) IsTunnelTypeAllowed(tunnelType string) bool {
	return containsFold(h.allowedTunnelTypes, tunnelType)
}

// containsFold returns true if the slice is empty (allow all) or contains the
// value in a case-insensitive comparison.
func containsFold(allowed []string, value string) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, a := range allowed {
		if strings.EqualFold(a, value) {
			return true
		}
	}
	return false
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
	if r.URL.Path == "/stats" {
		h.serveStats(w, r)
		return
	}
	if r.URL.Path == "/metrics" {
		h.serveMetrics(w, r)
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
		clientIP := h.extractClientIP(r)
		if !tconn.IsIPAllowed(clientIP) {
			http.Error(w, "Access denied: your IP is not allowed", http.StatusForbidden)
			return
		}
	}

	// Check proxy authentication
	if tconn.HasProxyAuth() {
		if r.URL.Path == "/_drip/login" {
			h.handleProxyLogin(w, r, tconn, subdomain)
			return
		}
		if !h.isProxyAuthenticated(r, subdomain) {
			h.serveLoginPage(w, r, subdomain, "")
			return
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

	if httputil.IsWebSocketUpgrade(r) {
		h.handleWebSocket(w, r, tconn)
		return
	}

	stream, err := h.openStreamWithTimeout(tconn)
	if err != nil {
		w.Header().Set("Connection", "close")
		http.Error(w, "Tunnel unavailable", http.StatusBadGateway)
		return
	}
	defer stream.Close()

	tconn.IncActiveConnections()
	defer tconn.DecActiveConnections()

	var limitedStream net.Conn = stream
	if limiter := tconn.GetLimiter(); limiter != nil && limiter.IsLimited() {
		limitedStream = qos.NewLimitedConn(r.Context(), stream, limiter)
	}

	countingStream := netutil.NewCountingConn(limitedStream,
		tconn.AddBytesOut,
		tconn.AddBytesIn,
	)

	if err := r.Write(countingStream); err != nil {
		w.Header().Set("Connection", "close")
		_ = r.Body.Close()
		http.Error(w, "Forward failed", http.StatusBadGateway)
		return
	}

	reader := bufioReaderPool.Get().(*bufio.Reader)
	reader.Reset(countingStream)
	resp, err := http.ReadResponse(reader, r)
	if err != nil {
		bufioReaderPool.Put(reader)
		w.Header().Set("Connection", "close")
		http.Error(w, "Read response failed", http.StatusBadGateway)
		return
	}
	defer func() {
		resp.Body.Close()
		bufioReaderPool.Put(reader)
	}()

	h.copyResponseHeaders(w.Header(), resp.Header, r.Host)

	statusCode := resp.StatusCode
	if statusCode == 0 {
		statusCode = http.StatusOK
	}

	if r.Method == http.MethodHead || statusCode == http.StatusNoContent || statusCode == http.StatusNotModified {
		if resp.ContentLength >= 0 {
			w.Header().Set("Content-Length", fmt.Sprintf("%d", resp.ContentLength))
		} else {
			w.Header().Del("Content-Length")
		}
		w.WriteHeader(statusCode)
		return
	}

	if resp.ContentLength >= 0 {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", resp.ContentLength))
	} else {
		w.Header().Del("Content-Length")
	}

	w.WriteHeader(statusCode)

	// Use pooled buffer for zero-copy optimization
	buf := pool.GetBuffer(pool.SizeLarge)
	defer pool.PutBuffer(buf)

	// Copy with context cancellation support
	ctx := r.Context()
	copyDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			stream.Close()
		case <-copyDone:
		}
	}()

	_, _ = io.CopyBuffer(w, resp.Body, (*buf)[:])
	close(copyDone)
}

func (h *Handler) openStreamWithTimeout(tconn *tunnel.Connection) (net.Conn, error) {
	type result struct {
		stream net.Conn
		err    error
	}
	ch := make(chan result, 1)

	go func() {
		s, err := tconn.OpenStream()
		ch <- result{s, err}
	}()

	select {
	case r := <-ch:
		return r.stream, r.err
	case <-time.After(openStreamTimeout):
		// Goroutine will eventually complete and send to buffered channel
		// which will be garbage collected. If stream was opened, it needs cleanup.
		go func() {
			if r := <-ch; r.stream != nil {
				r.stream.Close()
			}
		}()
		return nil, fmt.Errorf("open stream timeout")
	}
}

func (h *Handler) handleWebSocket(w http.ResponseWriter, r *http.Request, tconn *tunnel.Connection) {
	stream, err := h.openStreamWithTimeout(tconn)
	if err != nil {
		http.Error(w, "Tunnel unavailable", http.StatusBadGateway)
		return
	}

	tconn.IncActiveConnections()

	hj, ok := w.(http.Hijacker)
	if !ok {
		stream.Close()
		tconn.DecActiveConnections()
		http.Error(w, "WebSocket not supported", http.StatusInternalServerError)
		return
	}

	clientConn, clientBuf, err := hj.Hijack()
	if err != nil {
		stream.Close()
		tconn.DecActiveConnections()
		http.Error(w, "Failed to hijack connection", http.StatusInternalServerError)
		return
	}

	if err := r.Write(stream); err != nil {
		stream.Close()
		clientConn.Close()
		tconn.DecActiveConnections()
		return
	}

	var limitedStream net.Conn = stream
	if limiter := tconn.GetLimiter(); limiter != nil && limiter.IsLimited() {
		limitedStream = qos.NewLimitedConn(context.Background(), stream, limiter)
	}

	go func() {
		defer stream.Close()
		defer clientConn.Close()
		defer tconn.DecActiveConnections()

		var clientRW io.ReadWriteCloser = clientConn
		if clientBuf != nil && clientBuf.Reader.Buffered() > 0 {
			clientRW = &bufferedReadWriteCloser{
				Reader: clientBuf.Reader,
				Conn:   clientConn,
			}
		}

		_ = netutil.PipeWithCallbacks(context.Background(), limitedStream, clientRW,
			func(n int64) { tconn.AddBytesOut(n) },
			func(n int64) { tconn.AddBytesIn(n) },
		)
	}()
}

func (h *Handler) copyResponseHeaders(dst http.Header, src http.Header, proxyHost string) {
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

	if locationURL.Host == "localhost" ||
		strings.HasPrefix(locationURL.Host, "localhost:") ||
		locationURL.Host == "127.0.0.1" ||
		strings.HasPrefix(locationURL.Host, "127.0.0.1:") {
		rewritten := fmt.Sprintf("https://%s%s", proxyHost, locationURL.Path)
		if locationURL.RawQuery != "" {
			rewritten += "?" + locationURL.RawQuery
		}
		if locationURL.Fragment != "" {
			rewritten += "#" + locationURL.Fragment
		}
		return rewritten
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
	if idx := strings.Index(host, ":"); idx != -1 {
		host = host[:idx]
	}

	if host == h.serverDomain {
		return "", subdomainHome
	}

	suffix := "." + h.tunnelDomain
	if strings.HasSuffix(host, suffix) {
		return strings.TrimSuffix(host, suffix), subdomainFound
	}

	if host == h.tunnelDomain {
		return "", subdomainNotFound
	}

	return "", subdomainNotFound
}

// extractClientIP extracts the client IP from the request.
// It only trusts X-Forwarded-For and X-Real-IP headers when the request
// comes from a private/loopback network (typical reverse proxy setup).
func (h *Handler) extractClientIP(r *http.Request) string {
	// First, get the direct remote address
	remoteIP := h.extractRemoteIP(r.RemoteAddr)

	// Only trust proxy headers if the request comes from a private network
	if isPrivateIP(remoteIP) {
		// Check X-Forwarded-For header (may contain multiple IPs)
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			// Take the first IP (original client)
			if idx := strings.Index(xff, ","); idx != -1 {
				return strings.TrimSpace(xff[:idx])
			}
			return strings.TrimSpace(xff)
		}

		// Check X-Real-IP header
		if xri := r.Header.Get("X-Real-IP"); xri != "" {
			return strings.TrimSpace(xri)
		}
	}

	// Fall back to remote address
	return remoteIP
}

// extractRemoteIP extracts the IP address from a remote address string (host:port format).
func (h *Handler) extractRemoteIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}

// isPrivateIP checks if the given IP is a private/loopback address.
func isPrivateIP(ip string) bool {
	parsedIP := net.ParseIP(ip)
	if parsedIP == nil {
		return false
	}

	for _, network := range privateNetworks {
		if network.Contains(parsedIP) {
			return true
		}
	}

	return false
}

func (h *Handler) serveHomePage(w http.ResponseWriter, r *http.Request) {
	html := `<!DOCTYPE html>
<html lang="en">
<head>
	<meta charset="UTF-8" />
	<meta name="viewport" content="width=device-width, initial-scale=1.0" />
	<title>Drip - Your Tunnel, Your Domain, Anywhere</title>
	` + faviconLink + `
	<style>
		* { margin: 0; padding: 0; box-sizing: border-box; }
		body {
			font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif;
			background: #fff;
			color: #24292f;
			line-height: 1.6;
		}
		.container { max-width: 720px; margin: 0 auto; padding: 48px 24px; }
		header { margin-bottom: 48px; }
		h1 { font-size: 28px; font-weight: 600; margin-bottom: 8px; }
		h1 span { margin-right: 8px; }
		.desc { color: #57606a; font-size: 16px; }
		h2 { font-size: 18px; font-weight: 600; margin: 32px 0 12px; }
		.code-wrap {
			position: relative;
			background: #f6f8fa;
			border: 1px solid #d0d7de;
			border-radius: 6px;
			margin-bottom: 12px;
		}
		.code-wrap pre {
			margin: 0;
			padding: 12px 16px;
			padding-right: 60px;
			font-family: ui-monospace, SFMono-Regular, 'SF Mono', Menlo, Consolas, monospace;
			font-size: 14px;
			overflow-x: auto;
			white-space: pre-wrap;
			word-break: break-all;
		}
		.copy-btn {
			position: absolute;
			top: 8px;
			right: 8px;
			background: #fff;
			border: 1px solid #d0d7de;
			border-radius: 6px;
			padding: 4px 6px;
			cursor: pointer;
			color: #57606a;
			display: flex;
			align-items: center;
			justify-content: center;
		}
		.copy-btn:hover { background: #f3f4f6; }
		.copy-btn svg { width: 16px; height: 16px; }
		.copy-btn .check { display: none; color: #1a7f37; }
		.copy-btn.copied .copy { display: none; }
		.copy-btn.copied .check { display: block; }
		.links { margin-top: 32px; display: flex; gap: 24px; flex-wrap: wrap; }
		.links a { color: #0969da; text-decoration: none; font-size: 14px; }
		.links a:hover { text-decoration: underline; }
		footer { margin-top: 48px; padding-top: 24px; border-top: 1px solid #d0d7de; }
		footer a { color: #57606a; text-decoration: none; font-size: 14px; }
		footer a:hover { color: #0969da; }
	</style>
</head>
<body>
	<div class="container">
		<header>
			<h1><span>💧</span>Drip</h1>
			<p class="desc">Your Tunnel, Your Domain, Anywhere</p>
		</header>

		<p>A self-hosted tunneling solution to securely expose your services to the internet.</p>

		<h2>Install</h2>
		<div class="code-wrap">
			<pre>bash &lt;(curl -fsSL https://driptunnel.app/install.sh)</pre>
			<button class="copy-btn" onclick="copy(this)">
				<svg class="copy" viewBox="0 0 16 16" fill="currentColor"><path d="M0 6.75C0 5.784.784 5 1.75 5h1.5a.75.75 0 0 1 0 1.5h-1.5a.25.25 0 0 0-.25.25v7.5c0 .138.112.25.25.25h7.5a.25.25 0 0 0 .25-.25v-1.5a.75.75 0 0 1 1.5 0v1.5A1.75 1.75 0 0 1 9.25 16h-7.5A1.75 1.75 0 0 1 0 14.25Z"></path><path d="M5 1.75C5 .784 5.784 0 6.75 0h7.5C15.216 0 16 .784 16 1.75v7.5A1.75 1.75 0 0 1 14.25 11h-7.5A1.75 1.75 0 0 1 5 9.25Zm1.75-.25a.25.25 0 0 0-.25.25v7.5c0 .138.112.25.25.25h7.5a.25.25 0 0 0 .25-.25v-7.5a.25.25 0 0 0-.25-.25Z"></path></svg>
				<svg class="check" viewBox="0 0 16 16" fill="currentColor"><path d="M13.78 4.22a.75.75 0 0 1 0 1.06l-7.25 7.25a.75.75 0 0 1-1.06 0L2.22 9.28a.751.751 0 0 1 .018-1.042.751.751 0 0 1 1.042-.018L6 10.94l6.72-6.72a.75.75 0 0 1 1.06 0Z"></path></svg>
			</button>
		</div>

		<h2>Usage</h2>
		<div class="code-wrap">
			<pre>drip http 3000</pre>
			<button class="copy-btn" onclick="copy(this)">
				<svg class="copy" viewBox="0 0 16 16" fill="currentColor"><path d="M0 6.75C0 5.784.784 5 1.75 5h1.5a.75.75 0 0 1 0 1.5h-1.5a.25.25 0 0 0-.25.25v7.5c0 .138.112.25.25.25h7.5a.25.25 0 0 0 .25-.25v-1.5a.75.75 0 0 1 1.5 0v1.5A1.75 1.75 0 0 1 9.25 16h-7.5A1.75 1.75 0 0 1 0 14.25Z"></path><path d="M5 1.75C5 .784 5.784 0 6.75 0h7.5C15.216 0 16 .784 16 1.75v7.5A1.75 1.75 0 0 1 14.25 11h-7.5A1.75 1.75 0 0 1 5 9.25Zm1.75-.25a.25.25 0 0 0-.25.25v7.5c0 .138.112.25.25.25h7.5a.25.25 0 0 0 .25-.25v-7.5a.25.25 0 0 0-.25-.25Z"></path></svg>
				<svg class="check" viewBox="0 0 16 16" fill="currentColor"><path d="M13.78 4.22a.75.75 0 0 1 0 1.06l-7.25 7.25a.75.75 0 0 1-1.06 0L2.22 9.28a.751.751 0 0 1 .018-1.042.751.751 0 0 1 1.042-.018L6 10.94l6.72-6.72a.75.75 0 0 1 1.06 0Z"></path></svg>
			</button>
		</div>
		<div class="code-wrap">
			<pre>drip https 443</pre>
			<button class="copy-btn" onclick="copy(this)">
				<svg class="copy" viewBox="0 0 16 16" fill="currentColor"><path d="M0 6.75C0 5.784.784 5 1.75 5h1.5a.75.75 0 0 1 0 1.5h-1.5a.25.25 0 0 0-.25.25v7.5c0 .138.112.25.25.25h7.5a.25.25 0 0 0 .25-.25v-1.5a.75.75 0 0 1 1.5 0v1.5A1.75 1.75 0 0 1 9.25 16h-7.5A1.75 1.75 0 0 1 0 14.25Z"></path><path d="M5 1.75C5 .784 5.784 0 6.75 0h7.5C15.216 0 16 .784 16 1.75v7.5A1.75 1.75 0 0 1 14.25 11h-7.5A1.75 1.75 0 0 1 5 9.25Zm1.75-.25a.25.25 0 0 0-.25.25v7.5c0 .138.112.25.25.25h7.5a.25.25 0 0 0 .25-.25v-7.5a.25.25 0 0 0-.25-.25Z"></path></svg>
				<svg class="check" viewBox="0 0 16 16" fill="currentColor"><path d="M13.78 4.22a.75.75 0 0 1 0 1.06l-7.25 7.25a.75.75 0 0 1-1.06 0L2.22 9.28a.751.751 0 0 1 .018-1.042.751.751 0 0 1 1.042-.018L6 10.94l6.72-6.72a.75.75 0 0 1 1.06 0Z"></path></svg>
			</button>
		</div>
		<div class="code-wrap">
			<pre>drip tcp 5432</pre>
			<button class="copy-btn" onclick="copy(this)">
				<svg class="copy" viewBox="0 0 16 16" fill="currentColor"><path d="M0 6.75C0 5.784.784 5 1.75 5h1.5a.75.75 0 0 1 0 1.5h-1.5a.25.25 0 0 0-.25.25v7.5c0 .138.112.25.25.25h7.5a.25.25 0 0 0 .25-.25v-1.5a.75.75 0 0 1 1.5 0v1.5A1.75 1.75 0 0 1 9.25 16h-7.5A1.75 1.75 0 0 1 0 14.25Z"></path><path d="M5 1.75C5 .784 5.784 0 6.75 0h7.5C15.216 0 16 .784 16 1.75v7.5A1.75 1.75 0 0 1 14.25 11h-7.5A1.75 1.75 0 0 1 5 9.25Zm1.75-.25a.25.25 0 0 0-.25.25v7.5c0 .138.112.25.25.25h7.5a.25.25 0 0 0 .25-.25v-7.5a.25.25 0 0 0-.25-.25Z"></path></svg>
				<svg class="check" viewBox="0 0 16 16" fill="currentColor"><path d="M13.78 4.22a.75.75 0 0 1 0 1.06l-7.25 7.25a.75.75 0 0 1-1.06 0L2.22 9.28a.751.751 0 0 1 .018-1.042.751.751 0 0 1 1.042-.018L6 10.94l6.72-6.72a.75.75 0 0 1 1.06 0Z"></path></svg>
			</button>
		</div>

		<div class="links">
			<a href="/health">Health Check</a>
			<a href="/stats">Statistics</a>
			<a href="/metrics">Prometheus Metrics</a>
		</div>

		<footer>
			<a href="https://github.com/Gouryella/drip" target="_blank">GitHub</a>
		</footer>
	</div>
	<script>
	function copy(btn) {
		const text = btn.previousElementSibling.textContent;
		navigator.clipboard.writeText(text).then(() => {
			btn.classList.add('copied');
			setTimeout(() => { btn.classList.remove('copied'); }, 2000);
		});
	}
	</script>
</body>
</html>`

	data := []byte(html)
	w.Header().Set("Content-Type", "text/html")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
	w.Write(data)
}

func (h *Handler) serveTunnelNotFound(w http.ResponseWriter, r *http.Request) {
	html := `<!DOCTYPE html>
<html lang="en">
<head>
	<meta charset="UTF-8" />
	<meta name="viewport" content="width=device-width, initial-scale=1.0" />
	<title>404 - Tunnel Not Found</title>
	` + faviconLink + `
	<style>
		* { margin: 0; padding: 0; box-sizing: border-box; }
		body {
			font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif;
			background: #fff;
			color: #24292f;
			line-height: 1.6;
		}
		.container { max-width: 720px; margin: 0 auto; padding: 48px 24px; }
		header { margin-bottom: 48px; }
		h1 { font-size: 28px; font-weight: 600; margin-bottom: 8px; }
		h1 span { margin-right: 8px; }
		.desc { color: #57606a; font-size: 16px; }
		p { margin-bottom: 16px; }
		.info-box {
			background: #f6f8fa;
			border: 1px solid #d0d7de;
			border-radius: 6px;
			padding: 16px;
			margin: 24px 0;
		}
		.info-box ul {
			margin: 12px 0 0 20px;
			color: #57606a;
		}
		.info-box li { margin-bottom: 8px; }
		footer { margin-top: 48px; padding-top: 24px; border-top: 1px solid #d0d7de; }
		footer a { color: #57606a; text-decoration: none; font-size: 14px; }
		footer a:hover { color: #0969da; }
	</style>
</head>
<body>
	<div class="container">
		<header>
			<h1><span>🔍</span>Tunnel Not Found</h1>
			<p class="desc">The requested tunnel does not exist or has been closed.</p>
		</header>

		<div class="info-box">
			<p>This could happen because:</p>
			<ul>
				<li>The tunnel was never created</li>
				<li>The tunnel has been closed by the owner</li>
				<li>The tunnel URL is incorrect</li>
			</ul>
		</div>

		<p>If you are the tunnel owner, please restart your tunnel client.</p>

		<footer>
			<a href="https://github.com/Gouryella/drip" target="_blank">GitHub</a>
		</footer>
	</div>
</body>
</html>`

	data := []byte(html)
	w.Header().Set("Content-Type", "text/html")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
	w.WriteHeader(http.StatusNotFound)
	w.Write(data)
}

func (h *Handler) serveHealth(w http.ResponseWriter, r *http.Request) {
	health := map[string]interface{}{
		"status":         "ok",
		"active_tunnels": h.manager.Count(),
		"timestamp":      time.Now().Unix(),
	}

	data, err := json.Marshal(health)
	if err != nil {
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
	w.Write(data)
}

func (h *Handler) serveStats(w http.ResponseWriter, r *http.Request) {
	if h.metricsToken != "" {
		// Only accept token via Authorization header (Bearer token)
		// URL query parameters are insecure (logged, cached, visible in browser history)
		var token string
		authHeader := r.Header.Get("Authorization")
		if strings.HasPrefix(authHeader, "Bearer ") {
			token = strings.TrimPrefix(authHeader, "Bearer ")
		}

		if token != h.metricsToken {
			w.Header().Set("WWW-Authenticate", `Bearer realm="stats"`)
			http.Error(w, "Unauthorized: provide metrics token via 'Authorization: Bearer <token>' header", http.StatusUnauthorized)
			return
		}
	}

	connections := h.manager.List()

	// Pre-allocate slice to avoid O(n²) reallocations
	tunnelStats := make([]map[string]interface{}, 0, len(connections))
	for _, conn := range connections {
		if conn == nil {
			continue
		}
		tunnelStats = append(tunnelStats, map[string]interface{}{
			"subdomain":          conn.Subdomain,
			"tunnel_type":        string(conn.GetTunnelType()),
			"last_active":        conn.LastActive.Unix(),
			"bytes_in":           conn.GetBytesIn(),
			"bytes_out":          conn.GetBytesOut(),
			"active_connections": conn.GetActiveConnections(),
			"total_bytes":        conn.GetBytesIn() + conn.GetBytesOut(),
		})
	}

	stats := map[string]interface{}{
		"total_tunnels": len(tunnelStats),
		"tunnels":       tunnelStats,
	}

	data, err := json.Marshal(stats)
	if err != nil {
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
	w.Write(data)
}

func (h *Handler) serveMetrics(w http.ResponseWriter, r *http.Request) {
	if h.metricsToken != "" {
		// Only accept token via Authorization header (Bearer token)
		var token string
		authHeader := r.Header.Get("Authorization")
		if strings.HasPrefix(authHeader, "Bearer ") {
			token = strings.TrimPrefix(authHeader, "Bearer ")
		}

		if token != h.metricsToken {
			w.Header().Set("WWW-Authenticate", `Bearer realm="metrics"`)
			http.Error(w, "Unauthorized: provide metrics token via 'Authorization: Bearer <token>' header", http.StatusUnauthorized)
			return
		}
	}

	// Serve Prometheus metrics
	promhttp.Handler().ServeHTTP(w, r)
}

type bufferedReadWriteCloser struct {
	*bufio.Reader
	net.Conn
}

func (b *bufferedReadWriteCloser) Read(p []byte) (int, error) {
	return b.Reader.Read(p)
}

func (h *Handler) isProxyAuthenticated(r *http.Request, subdomain string) bool {
	cookie, err := r.Cookie(authCookieName + "_" + subdomain)
	if err != nil {
		return false
	}
	return sessionStore.validate(cookie.Value, subdomain)
}

func (h *Handler) handleProxyLogin(w http.ResponseWriter, r *http.Request, tconn *tunnel.Connection, subdomain string) {
	if r.Method != http.MethodPost {
		h.serveLoginPage(w, r, subdomain, "")
		return
	}

	if err := r.ParseForm(); err != nil {
		h.serveLoginPage(w, r, subdomain, "Invalid form data")
		return
	}

	password := r.FormValue("password")

	if !tconn.ValidateProxyAuth(password) {
		h.serveLoginPage(w, r, subdomain, "Invalid password")
		return
	}

	token := sessionStore.create(subdomain)
	http.SetCookie(w, &http.Cookie{
		Name:     authCookieName + "_" + subdomain,
		Value:    token,
		Path:     "/",
		MaxAge:   int(authSessionDuration.Seconds()),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})

	redirectURL := r.FormValue("redirect")
	if redirectURL == "" || redirectURL == "/_drip/login" {
		redirectURL = "/"
	}
	http.Redirect(w, r, redirectURL, http.StatusSeeOther)
}

func (h *Handler) serveLoginPage(w http.ResponseWriter, r *http.Request, subdomain string, errorMsg string) {
	redirectURL := r.URL.Path
	if r.URL.RawQuery != "" {
		redirectURL += "?" + r.URL.RawQuery
	}
	if redirectURL == "/_drip/login" {
		redirectURL = "/"
	}

	errorHTML := ""
	if errorMsg != "" {
		errorHTML = fmt.Sprintf(`<p class="error">%s</p>`, html.EscapeString(errorMsg))
	}

	safeRedirectURL := html.EscapeString(redirectURL)

	htmlContent := fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head>
	<meta charset="UTF-8" />
	<meta name="viewport" content="width=device-width, initial-scale=1.0" />
	<title>%s - Drip</title>
	`+faviconLink+`
	<style>
		* { margin: 0; padding: 0; box-sizing: border-box; }
		body {
			font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif;
			background: #fff;
			color: #24292f;
			line-height: 1.6;
		}
		.container { max-width: 720px; margin: 0 auto; padding: 48px 24px; }
		header { margin-bottom: 48px; }
		h1 { font-size: 28px; font-weight: 600; margin-bottom: 8px; }
		h1 span { margin-right: 8px; }
		.desc { color: #57606a; font-size: 16px; }
		p { margin-bottom: 24px; }
		.error { color: #cf222e; margin-bottom: 16px; }
		.input-wrap {
			position: relative;
			background: #f6f8fa;
			border: 1px solid #d0d7de;
			border-radius: 6px;
			margin-bottom: 12px;
			display: flex;
		}
		.input-wrap input {
			flex: 1;
			margin: 0;
			padding: 12px 16px;
			font-family: ui-monospace, SFMono-Regular, 'SF Mono', Menlo, Consolas, monospace;
			font-size: 14px;
			background: transparent;
			border: none;
			outline: none;
		}
		.input-wrap button {
			background: #24292f;
			color: #fff;
			border: none;
			padding: 8px 16px;
			margin: 4px;
			border-radius: 4px;
			font-size: 14px;
			cursor: pointer;
		}
		.input-wrap button:hover { background: #32383f; }
		footer { margin-top: 48px; padding-top: 24px; border-top: 1px solid #d0d7de; }
		footer a { color: #57606a; text-decoration: none; font-size: 14px; }
		footer a:hover { color: #0969da; }
	</style>
</head>
<body>
	<div class="container">
		<header>
			<h1><span>🔒</span>%s</h1>
			<p class="desc">This tunnel is password protected</p>
		</header>

		%s
		<form method="POST" action="/_drip/login">
			<input type="hidden" name="redirect" value="%s" />
			<div class="input-wrap">
				<input type="password" name="password" placeholder="Enter password" required autofocus />
				<button type="submit">Continue</button>
			</div>
		</form>

		<footer>
			<a href="https://github.com/Gouryella/drip" target="_blank">GitHub</a>
		</footer>
	</div>
</body>
</html>`, subdomain, subdomain, errorHTML, safeRedirectURL)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.WriteHeader(http.StatusUnauthorized)
	w.Write([]byte(htmlContent))
}

// handleTunnelWebSocket handles WebSocket connections for tunnel clients
func (h *Handler) handleTunnelWebSocket(w http.ResponseWriter, r *http.Request) {
	// Check if WSS transport is allowed
	if !h.IsTransportAllowed("wss") {
		http.Error(w, "WebSocket transport not allowed on this server", http.StatusForbidden)
		return
	}

	if h.wsConnHandler == nil {
		http.Error(w, "WebSocket tunnel not configured", http.StatusServiceUnavailable)
		return
	}

	ws, err := h.wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		h.logger.Error("WebSocket upgrade failed", zap.Error(err))
		return
	}

	// Configure WebSocket for tunnel use
	ws.SetReadLimit(protocol.MaxFrameSize + protocol.FrameHeaderSize + 1024)

	// Extract real client IP (support CDN headers)
	remoteAddr := h.extractClientIP(r)

	h.logger.Info("WebSocket tunnel connection established",
		zap.String("remote_addr", remoteAddr),
	)

	// Wrap WebSocket as net.Conn with ping loop for CDN keep-alive
	conn := wsutil.NewConnWithPing(ws, 30*time.Second)

	// Handle the connection using the registered handler
	h.wsConnHandler.HandleWSConnection(conn, remoteAddr)
}

// serveDiscovery returns server capabilities for client auto-detection
func (h *Handler) serveDiscovery(w http.ResponseWriter, r *http.Request) {
	transports := h.allowedTransports
	if len(transports) == 0 {
		transports = []string{"tcp", "wss"}
	}

	tunnelTypes := h.allowedTunnelTypes
	if len(tunnelTypes) == 0 {
		tunnelTypes = []string{"http", "https", "tcp"}
	}

	response := map[string]interface{}{
		"transports":   transports,
		"tunnel_types": tunnelTypes,
		"preferred":    h.GetPreferredTransport(),
		"version":      "1",
	}

	data, err := json.Marshal(response)
	if err != nil {
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(data)
}
