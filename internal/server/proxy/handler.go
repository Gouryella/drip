package proxy

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	json "github.com/goccy/go-json"

	"drip/internal/server/tunnel"
	"drip/internal/shared/httputil"
	"drip/internal/shared/netutil"
	"drip/internal/shared/protocol"

	"go.uber.org/zap"
)

const openStreamTimeout = 10 * time.Second

type Handler struct {
	manager   *tunnel.Manager
	logger    *zap.Logger
	domain    string
	authToken string
}

func NewHandler(manager *tunnel.Manager, logger *zap.Logger, domain string, authToken string) *Handler {
	return &Handler{
		manager:   manager,
		logger:    logger,
		domain:    domain,
		authToken: authToken,
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

	resp, err := http.ReadResponse(bufio.NewReaderSize(countingStream, 32*1024), r)
	if err != nil {
		w.Header().Set("Connection", "close")
		http.Error(w, "Read response failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

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

	ctx := r.Context()
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			stream.Close()
		case <-done:
		}
	}()

	_, _ = io.Copy(w, resp.Body)
	close(done)
	stream.Close()
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

	clientConn, _, err := hj.Hijack()
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

		_ = netutil.PipeWithCallbacks(r.Context(), stream, clientConn,
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

func (h *Handler) serveHomePage(w http.ResponseWriter, r *http.Request) {
	html := `<!DOCTYPE html>
<html>
<head>
	<meta charset="UTF-8" />
    <title>Drip - Your Tunnel, Your Domain, Anywhere</title>
    <style>
        body { font-family: Arial, sans-serif; max-width: 800px; margin: 50px auto; padding: 20px; }
        h1 { color: #333; }
        code { background: #f4f4f4; padding: 2px 6px; border-radius: 3px; }
        .stats { background: #f9f9f9; padding: 15px; border-radius: 5px; margin: 20px 0; }
    </style>
</head>
<body>
    <h1>💧 Drip - Your Tunnel, Your Domain, Anywhere</h1>
    <p>A self-hosted tunneling solution to securely expose your services to the internet.</p>

    <h2>Quick Start</h2>
    <p>Install the client:</p>
    <code>bash <(curl -fsSL https://raw.githubusercontent.com/Gouryella/drip/main/scripts/install.sh)</code>

    <p>Start a tunnel:</p>
	<code>drip http 3000</code><br><br>
	<code>drip https 443</code><br><br>
	<code>drip tcp 5432</code>
    <p><a href="/health">Health Check</a> | <a href="/stats">Statistics</a></p>
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
	if h.authToken != "" {
		token := r.URL.Query().Get("token")
		if token == "" {
			authHeader := r.Header.Get("Authorization")
			if strings.HasPrefix(authHeader, "Bearer ") {
				token = strings.TrimPrefix(authHeader, "Bearer ")
			}
		}

		if token != h.authToken {
			http.Error(w, "Unauthorized: invalid or missing token", http.StatusUnauthorized)
			return
		}
	}

	connections := h.manager.List()

	stats := map[string]interface{}{
		"total_tunnels": len(connections),
		"tunnels":       []map[string]interface{}{},
	}

	for _, conn := range connections {
		if conn == nil {
			continue
		}
		stats["tunnels"] = append(stats["tunnels"].([]map[string]interface{}), map[string]interface{}{
			"subdomain":          conn.Subdomain,
			"tunnel_type":        string(conn.GetTunnelType()),
			"last_active":        conn.LastActive.Unix(),
			"bytes_in":           conn.GetBytesIn(),
			"bytes_out":          conn.GetBytesOut(),
			"active_connections": conn.GetActiveConnections(),
			"total_bytes":        conn.GetBytesIn() + conn.GetBytesOut(),
		})
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
