package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	json "github.com/goccy/go-json"

	"drip/internal/server/tunnel"
	"drip/internal/shared/httputil"
	"drip/internal/shared/netutil"
	"drip/internal/shared/pool"
	"drip/internal/shared/protocol"

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

type Handler struct {
	manager      *tunnel.Manager
	logger       *zap.Logger
	domain       string
	authToken    string
	metricsToken string
}

var privateNetworks []*net.IPNet

func init() {
	privateCIDRs := []string{
		"127.0.0.0/8",    // IPv4 loopback
		"10.0.0.0/8",     // RFC 1918 Class A
		"172.16.0.0/12",  // RFC 1918 Class B
		"192.168.0.0/16", // RFC 1918 Class C
		"::1/128",        // IPv6 loopback
		"fc00::/7",       // IPv6 unique local
		"fe80::/10",      // IPv6 link-local
	}
	for _, cidr := range privateCIDRs {
		_, ipNet, _ := net.ParseCIDR(cidr)
		privateNetworks = append(privateNetworks, ipNet)
	}
}

func NewHandler(manager *tunnel.Manager, logger *zap.Logger, domain string, authToken string, metricsToken string) *Handler {
	return &Handler{
		manager:      manager,
		logger:       logger,
		domain:       domain,
		authToken:    authToken,
		metricsToken: metricsToken,
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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

	subdomain := h.extractSubdomain(r.Host)
	if subdomain == "" {
		h.serveHomePage(w, r)
		return
	}

	tconn, ok := h.manager.Get(subdomain)
	if !ok || tconn == nil {
		http.Error(w, "Tunnel not found. The tunnel may have been closed.", http.StatusNotFound)
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

	countingStream := netutil.NewCountingConn(stream,
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

		_ = netutil.PipeWithCallbacks(context.Background(), stream, clientRW,
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

func (h *Handler) extractSubdomain(host string) string {
	if idx := strings.Index(host, ":"); idx != -1 {
		host = host[:idx]
	}

	if host == h.domain {
		return ""
	}

	suffix := "." + h.domain
	if strings.HasSuffix(host, suffix) {
		return strings.TrimSuffix(host, suffix)
	}

	return ""
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
			<pre>bash &lt;(curl -fsSL https://raw.githubusercontent.com/Gouryella/drip/main/scripts/install.sh)</pre>
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
