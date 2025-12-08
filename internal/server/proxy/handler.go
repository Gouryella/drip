package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	json "github.com/goccy/go-json"

	"drip/internal/server/tunnel"
	"drip/internal/shared/pool"
	"drip/internal/shared/protocol"
	"drip/internal/shared/utils"

	"go.uber.org/zap"
)

type Handler struct {
	manager    *tunnel.Manager
	logger     *zap.Logger
	responses  *ResponseHandler
	domain     string
	authToken  string
	headerPool *pool.HeaderPool
	bufferPool *pool.AdaptiveBufferPool
}

func NewHandler(manager *tunnel.Manager, logger *zap.Logger, responses *ResponseHandler, domain string, authToken string) *Handler {
	return &Handler{
		manager:    manager,
		logger:     logger,
		responses:  responses,
		domain:     domain,
		authToken:  authToken,
		headerPool: pool.NewHeaderPool(),
		bufferPool: pool.NewAdaptiveBufferPool(),
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Always handle /health and /stats directly, regardless of subdomain
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

	conn, ok := h.manager.Get(subdomain)
	if !ok {
		http.Error(w, "Tunnel not found. The tunnel may have been closed.", http.StatusNotFound)
		return
	}

	if conn.IsClosed() {
		http.Error(w, "Tunnel connection closed", http.StatusBadGateway)
		return
	}

	transport := conn.GetTransport()
	if transport == nil {
		http.Error(w, "Tunnel control channel not ready", http.StatusBadGateway)
		return
	}

	tType := conn.GetTunnelType()
	if tType != "" && tType != protocol.TunnelTypeHTTP && tType != protocol.TunnelTypeHTTPS {
		http.Error(w, "Tunnel does not accept HTTP traffic", http.StatusBadGateway)
		return
	}

	requestID := utils.GenerateID()

	h.handleAdaptiveRequest(w, r, transport, requestID, subdomain)
}

func (h *Handler) handleAdaptiveRequest(w http.ResponseWriter, r *http.Request, transport tunnel.Transport, requestID string, subdomain string) {
	const streamingThreshold int64 = 1 * 1024 * 1024

	ctx := r.Context()

	var cancelTransport func()
	if transport != nil {
		cancelOnce := sync.Once{}
		cancelFunc := func() {
			header := protocol.DataHeader{
				StreamID:  requestID,
				RequestID: requestID,
				Type:      protocol.DataTypeClose,
				IsLast:    true,
			}

			payload, poolBuffer, err := protocol.EncodeDataPayloadPooled(header, nil)
			if err != nil {
				return
			}

			frame := protocol.NewFramePooled(protocol.FrameTypeData, payload, poolBuffer)
			if err := transport.SendFrame(frame); err != nil {
				h.logger.Debug("Failed to send cancel frame to client",
					zap.String("request_id", requestID),
					zap.Error(err),
				)
			}
		}

		cancelTransport = func() {
			cancelOnce.Do(cancelFunc)
		}

		h.responses.RegisterCancelFunc(requestID, cancelTransport)
		defer h.responses.CleanupCancelFunc(requestID)
	}

	largeBufferPtr := h.bufferPool.GetLarge()
	tempBufPtr := h.bufferPool.GetMedium()

	defer func() {
		h.bufferPool.PutLarge(largeBufferPtr)
		h.bufferPool.PutMedium(tempBufPtr)
	}()

	buffer := (*largeBufferPtr)[:0]
	tempBuf := (*tempBufPtr)[:pool.MediumBufferSize]

	var totalRead int64
	var hitThreshold bool

	for totalRead < streamingThreshold {
		n, err := r.Body.Read(tempBuf)
		if n > 0 {
			buffer = append(buffer, tempBuf[:n]...)
			totalRead += int64(n)
		}
		if err == io.EOF {
			r.Body.Close()
			h.sendBufferedRequest(ctx, w, r, transport, requestID, subdomain, cancelTransport, buffer)
			return
		}
		if err != nil {
			r.Body.Close()
			h.logger.Error("Read request body failed", zap.Error(err))
			http.Error(w, "Failed to read request body", http.StatusInternalServerError)
			return
		}
		if totalRead >= streamingThreshold {
			hitThreshold = true
			break
		}
	}

	if !hitThreshold {
		r.Body.Close()
		h.sendBufferedRequest(ctx, w, r, transport, requestID, subdomain, cancelTransport, buffer)
		return
	}

	h.streamLargeRequest(ctx, w, r, transport, requestID, subdomain, cancelTransport, buffer)
}

func (h *Handler) sendBufferedRequest(ctx context.Context, w http.ResponseWriter, r *http.Request, transport tunnel.Transport, requestID string, subdomain string, cancelTransport func(), body []byte) {
	headers := h.headerPool.Get()
	h.headerPool.CloneWithExtra(headers, r.Header, "Host", r.Host)

	httpReq := protocol.HTTPRequest{
		Method:  r.Method,
		URL:     r.URL.String(),
		Headers: headers,
		Body:    body,
	}

	reqBytes, err := protocol.EncodeHTTPRequest(&httpReq)
	h.headerPool.Put(headers)

	if err != nil {
		h.logger.Error("Encode HTTP request failed", zap.Error(err))
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	header := protocol.DataHeader{
		StreamID:  requestID,
		RequestID: requestID,
		Type:      protocol.DataTypeHTTPRequest,
		IsLast:    true,
	}

	payload, poolBuffer, err := protocol.EncodeDataPayloadPooled(header, reqBytes)
	if err != nil {
		h.logger.Error("Encode data payload failed", zap.Error(err))
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	frame := protocol.NewFramePooled(protocol.FrameTypeData, payload, poolBuffer)

	respChan := h.responses.CreateResponseChan(requestID)
	streamingDone := h.responses.CreateStreamingResponse(requestID, w)
	defer func() {
		h.responses.CleanupResponseChan(requestID)
		h.responses.CleanupStreamingResponse(requestID)
	}()

	if err := transport.SendFrame(frame); err != nil {
		h.logger.Error("Send frame to tunnel failed", zap.Error(err))
		http.Error(w, "Failed to forward request to tunnel", http.StatusBadGateway)
		return
	}

	select {
	case respMsg := <-respChan:
		if respMsg == nil {
			http.Error(w, "Internal server error: nil response", http.StatusInternalServerError)
			return
		}
		h.writeHTTPResponse(w, respMsg, subdomain, r)
	case <-streamingDone:
		// Streaming response has been fully written by SendStreamingChunk
	case <-ctx.Done():
		if cancelTransport != nil {
			cancelTransport()
		}
		h.logger.Debug("HTTP request context cancelled",
			zap.String("request_id", requestID),
			zap.String("subdomain", subdomain),
		)
		return
	case <-time.After(5 * time.Minute):
		h.logger.Error("Request timeout",
			zap.String("request_id", requestID),
			zap.String("url", r.URL.String()),
		)
		http.Error(w, "Request timeout - the tunnel client did not respond in time", http.StatusGatewayTimeout)
	}
}

func (h *Handler) streamLargeRequest(ctx context.Context, w http.ResponseWriter, r *http.Request, transport tunnel.Transport, requestID string, subdomain string, cancelTransport func(), bufferedData []byte) {
	headers := h.headerPool.Get()
	h.headerPool.CloneWithExtra(headers, r.Header, "Host", r.Host)

	httpReqHead := protocol.HTTPRequestHead{
		Method:        r.Method,
		URL:           r.URL.String(),
		Headers:       headers,
		ContentLength: r.ContentLength,
	}

	headBytes, err := protocol.EncodeHTTPRequestHead(&httpReqHead)
	h.headerPool.Put(headers)

	if err != nil {
		h.logger.Error("Encode HTTP request head failed", zap.Error(err))
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	headHeader := protocol.DataHeader{
		StreamID:  requestID,
		RequestID: requestID,
		Type:      protocol.DataTypeHTTPHead, // shared streaming head type
		IsLast:    false,
	}

	headPayload, headPoolBuffer, err := protocol.EncodeDataPayloadPooled(headHeader, headBytes)
	if err != nil {
		h.logger.Error("Encode head payload failed", zap.Error(err))
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	headFrame := protocol.NewFramePooled(protocol.FrameTypeData, headPayload, headPoolBuffer)

	respChan := h.responses.CreateResponseChan(requestID)
	streamingDone := h.responses.CreateStreamingResponse(requestID, w)
	defer func() {
		h.responses.CleanupResponseChan(requestID)
		h.responses.CleanupStreamingResponse(requestID)
	}()

	if err := transport.SendFrame(headFrame); err != nil {
		h.logger.Error("Send head frame failed", zap.Error(err))
		http.Error(w, "Failed to forward request to tunnel", http.StatusBadGateway)
		return
	}

	if len(bufferedData) > 0 {
		chunkHeader := protocol.DataHeader{
			StreamID:  requestID,
			RequestID: requestID,
			Type:      protocol.DataTypeHTTPBodyChunk, // shared streaming body type
			IsLast:    false,
		}

		chunkPayload, chunkPoolBuffer, err := protocol.EncodeDataPayloadPooled(chunkHeader, bufferedData)
		if err != nil {
			h.logger.Error("Encode buffered chunk failed", zap.Error(err))

			finalHeader := protocol.DataHeader{
				StreamID:  requestID,
				RequestID: requestID,
				Type:      protocol.DataTypeHTTPRequestBodyChunk,
				IsLast:    true,
			}
			finalPayload, finalPoolBuffer, ferr := protocol.EncodeDataPayloadPooled(finalHeader, nil)
			if ferr == nil {
				finalFrame := protocol.NewFramePooled(protocol.FrameTypeData, finalPayload, finalPoolBuffer)
				transport.SendFrame(finalFrame)
			}
			return
		}

		chunkFrame := protocol.NewFramePooled(protocol.FrameTypeData, chunkPayload, chunkPoolBuffer)
		if err := transport.SendFrame(chunkFrame); err != nil {
			h.logger.Error("Send buffered chunk failed", zap.Error(err))

			finalHeader := protocol.DataHeader{
				StreamID:  requestID,
				RequestID: requestID,
				Type:      protocol.DataTypeHTTPRequestBodyChunk,
				IsLast:    true,
			}
			finalPayload, finalPoolBuffer, ferr := protocol.EncodeDataPayloadPooled(finalHeader, nil)
			if ferr == nil {
				finalFrame := protocol.NewFramePooled(protocol.FrameTypeData, finalPayload, finalPoolBuffer)
				transport.SendFrame(finalFrame)
			}
			return
		}
	}

	streamBufPtr := h.bufferPool.GetMedium()
	defer h.bufferPool.PutMedium(streamBufPtr)
	buffer := (*streamBufPtr)[:pool.MediumBufferSize]
	for {
		select {
		case <-ctx.Done():
			if cancelTransport != nil {
				cancelTransport()
			}
			h.logger.Debug("Streaming request cancelled via context",
				zap.String("request_id", requestID),
				zap.String("subdomain", subdomain),
			)
			return
		default:
		}

		n, readErr := r.Body.Read(buffer)
		if n > 0 {
			isLast := readErr == io.EOF

			chunkHeader := protocol.DataHeader{
				StreamID:  requestID,
				RequestID: requestID,
				Type:      protocol.DataTypeHTTPBodyChunk, // shared streaming body type
				IsLast:    isLast,
			}

			chunkPayload, chunkPoolBuffer, err := protocol.EncodeDataPayloadPooled(chunkHeader, buffer[:n])
			if err != nil {
				h.logger.Error("Encode chunk payload failed", zap.Error(err))

				finalHeader := protocol.DataHeader{
					StreamID:  requestID,
					RequestID: requestID,
					Type:      protocol.DataTypeHTTPRequestBodyChunk,
					IsLast:    true,
				}
				finalPayload, finalPoolBuffer, ferr := protocol.EncodeDataPayloadPooled(finalHeader, nil)
				if ferr == nil {
					finalFrame := protocol.NewFramePooled(protocol.FrameTypeData, finalPayload, finalPoolBuffer)
					transport.SendFrame(finalFrame)
				}
				return
			}

			chunkFrame := protocol.NewFramePooled(protocol.FrameTypeData, chunkPayload, chunkPoolBuffer)
			if err := transport.SendFrame(chunkFrame); err != nil {
				h.logger.Error("Send chunk frame failed", zap.Error(err))

				finalHeader := protocol.DataHeader{
					StreamID:  requestID,
					RequestID: requestID,
					Type:      protocol.DataTypeHTTPRequestBodyChunk,
					IsLast:    true,
				}
				finalPayload, finalPoolBuffer, ferr := protocol.EncodeDataPayloadPooled(finalHeader, nil)
				if ferr == nil {
					finalFrame := protocol.NewFramePooled(protocol.FrameTypeData, finalPayload, finalPoolBuffer)
					transport.SendFrame(finalFrame)
				}
				return
			}
		}

		if readErr == io.EOF {
			if n == 0 {
				finalHeader := protocol.DataHeader{
					StreamID:  requestID,
					RequestID: requestID,
					Type:      protocol.DataTypeHTTPRequestBodyChunk,
					IsLast:    true,
				}
				finalPayload, finalPoolBuffer, err := protocol.EncodeDataPayloadPooled(finalHeader, nil)
				if err == nil {
					finalFrame := protocol.NewFramePooled(protocol.FrameTypeData, finalPayload, finalPoolBuffer)
					transport.SendFrame(finalFrame)
				}
			}
			break
		}
		if readErr != nil {
			h.logger.Error("Read request body failed", zap.Error(readErr))

			finalHeader := protocol.DataHeader{
				StreamID:  requestID,
				RequestID: requestID,
				Type:      protocol.DataTypeHTTPRequestBodyChunk,
				IsLast:    true,
			}
			finalPayload, finalPoolBuffer, err := protocol.EncodeDataPayloadPooled(finalHeader, nil)
			if err == nil {
				finalFrame := protocol.NewFramePooled(protocol.FrameTypeData, finalPayload, finalPoolBuffer)
				transport.SendFrame(finalFrame)
			}

			http.Error(w, "Failed to read request body", http.StatusInternalServerError)
			return
		}
	}

	r.Body.Close()

	select {
	case respMsg := <-respChan:
		if respMsg == nil {
			http.Error(w, "Internal server error: nil response", http.StatusInternalServerError)
			return
		}
		h.writeHTTPResponse(w, respMsg, subdomain, r)
	case <-streamingDone:
		// Streaming response has been fully written by SendStreamingChunk
	case <-ctx.Done():
		if cancelTransport != nil {
			cancelTransport()
		}
		h.logger.Debug("Streaming HTTP request context cancelled",
			zap.String("request_id", requestID),
			zap.String("subdomain", subdomain),
		)
		return
	case <-time.After(5 * time.Minute):
		h.logger.Error("Streaming request timeout",
			zap.String("request_id", requestID),
			zap.String("url", r.URL.String()),
		)
		http.Error(w, "Request timeout - the tunnel client did not respond in time", http.StatusGatewayTimeout)
	}
}

func (h *Handler) writeHTTPResponse(w http.ResponseWriter, resp *protocol.HTTPResponse, subdomain string, r *http.Request) {
	if resp == nil {
		http.Error(w, "Invalid response from tunnel", http.StatusBadGateway)
		return
	}

	// For buffered responses, we have the complete body, so we can set Content-Length
	// Skip ALL hop-by-hop headers - client should have already cleaned them
	for key, values := range resp.Headers {
		canonicalKey := http.CanonicalHeaderKey(key)

		// Skip hop-by-hop headers completely using canonical key comparison
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
			rewrittenLocation := h.rewriteLocationHeader(values[0], r.Host)
			w.Header().Set("Location", rewrittenLocation)
			continue
		}

		for _, value := range values {
			w.Header().Add(key, value)
		}
	}

	// For buffered mode, always set Content-Length with the actual body size
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(resp.Body)))

	statusCode := resp.StatusCode
	if statusCode == 0 {
		statusCode = http.StatusOK
	}

	w.WriteHeader(statusCode)

	if len(resp.Body) > 0 {
		w.Write(resp.Body)
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
		scheme := "https"
		if strings.Contains(proxyHost, ":") && !strings.Contains(proxyHost, "https") {
			parts := strings.Split(proxyHost, ":")
			if len(parts) == 2 && parts[1] != "443" {
				scheme = "https"
			}
		}

		rewritten := fmt.Sprintf("%s://%s%s", scheme, proxyHost, locationURL.Path)
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
		subdomain := strings.TrimSuffix(host, suffix)
		return subdomain
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

	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(html))
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
		stats["tunnels"] = append(stats["tunnels"].([]map[string]interface{}), map[string]interface{}{
			"subdomain":   conn.Subdomain,
			"last_active": conn.LastActive.Unix(),
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
