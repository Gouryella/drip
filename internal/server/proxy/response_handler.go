package proxy

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"drip/internal/shared/protocol"

	"go.uber.org/zap"
)

// responseChanEntry holds a response channel and its creation time
type responseChanEntry struct {
	ch        chan *protocol.HTTPResponse
	createdAt time.Time
}

// streamingResponseEntry holds a streaming response writer
type streamingResponseEntry struct {
	w              http.ResponseWriter
	flusher        http.Flusher
	createdAt      time.Time
	lastActivityAt time.Time
	headersSent    bool
	done           chan struct{}
	mu             sync.Mutex
}

// ResponseHandler manages response channels for HTTP requests over TCP/Frame protocol
type ResponseHandler struct {
	channels          map[string]*responseChanEntry
	streamingChannels map[string]*streamingResponseEntry
	cancelFuncs       map[string]func()
	mu                sync.RWMutex
	logger            *zap.Logger
	stopCh            chan struct{}
}

// NewResponseHandler creates a new response handler
func NewResponseHandler(logger *zap.Logger) *ResponseHandler {
	h := &ResponseHandler{
		channels:          make(map[string]*responseChanEntry),
		streamingChannels: make(map[string]*streamingResponseEntry),
		cancelFuncs:       make(map[string]func()),
		logger:            logger,
		stopCh:            make(chan struct{}),
	}

	// Start single cleanup goroutine instead of one per request
	go h.cleanupLoop()

	return h
}

// CreateResponseChan creates a response channel for a request ID
func (h *ResponseHandler) CreateResponseChan(requestID string) chan *protocol.HTTPResponse {
	h.mu.Lock()
	defer h.mu.Unlock()

	ch := make(chan *protocol.HTTPResponse, 1)
	h.channels[requestID] = &responseChanEntry{
		ch:        ch,
		createdAt: time.Now(),
	}

	return ch
}

// CreateStreamingResponse creates a streaming response entry for a request ID
func (h *ResponseHandler) CreateStreamingResponse(requestID string, w http.ResponseWriter) chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()

	flusher, _ := w.(http.Flusher)
	done := make(chan struct{})
	now := time.Now()
	h.streamingChannels[requestID] = &streamingResponseEntry{
		w:              w,
		flusher:        flusher,
		createdAt:      now,
		lastActivityAt: now,
		done:           done,
	}

	return done
}

// RegisterCancelFunc registers a callback to be invoked when the downstream disconnects.
func (h *ResponseHandler) RegisterCancelFunc(requestID string, cancel func()) {
	if cancel == nil {
		return
	}

	h.mu.Lock()
	h.cancelFuncs[requestID] = cancel
	h.mu.Unlock()
}

// GetResponseChan gets the response channel for a request ID
func (h *ResponseHandler) GetResponseChan(requestID string) <-chan *protocol.HTTPResponse {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if entry := h.channels[requestID]; entry != nil {
		return entry.ch
	}
	return nil
}

// SendResponse sends a response to the waiting channel
func (h *ResponseHandler) SendResponse(requestID string, resp *protocol.HTTPResponse) {
	h.mu.RLock()
	entry, exists := h.channels[requestID]
	h.mu.RUnlock()

	if !exists || entry == nil {
		return
	}

	select {
	case entry.ch <- resp:
	case <-time.After(30 * time.Second):
		h.logger.Error("Timeout sending response to channel - handler may have abandoned",
			zap.String("request_id", requestID),
			zap.Int("status_code", resp.StatusCode),
			zap.Int("body_size", len(resp.Body)),
		)
	}
}

func (h *ResponseHandler) SendStreamingHead(requestID string, head *protocol.HTTPResponseHead) error {
	h.mu.RLock()
	entry, exists := h.streamingChannels[requestID]
	h.mu.RUnlock()

	if !exists || entry == nil {
		return nil
	}

	entry.mu.Lock()
	defer entry.mu.Unlock()

	select {
	case <-entry.done:
		return nil
	default:
	}

	if entry.headersSent {
		return nil
	}

	// Copy headers, removing hop-by-hop headers that were already handled by client
	// Client's cleanResponseHeaders already removed Transfer-Encoding, Connection, etc.
	// But we need to check again in case they slipped through
	hasContentLength := false

	for key, values := range head.Headers {
		canonicalKey := http.CanonicalHeaderKey(key)

		// Skip ALL hop-by-hop headers
		if canonicalKey == "Connection" ||
			canonicalKey == "Keep-Alive" ||
			canonicalKey == "Transfer-Encoding" ||
			canonicalKey == "Upgrade" ||
			canonicalKey == "Proxy-Connection" ||
			canonicalKey == "Te" ||
			canonicalKey == "Trailer" {
			continue
		}

		if canonicalKey == "Content-Length" {
			hasContentLength = true
		}

		for _, value := range values {
			entry.w.Header().Add(key, value)
		}
	}

	// For streaming responses, decide how to indicate message length
	if head.ContentLength >= 0 && !hasContentLength {
		entry.w.Header().Set("Content-Length", fmt.Sprintf("%d", head.ContentLength))
	}

	statusCode := head.StatusCode
	if statusCode == 0 {
		statusCode = http.StatusOK
	}

	entry.w.WriteHeader(statusCode)
	entry.headersSent = true
	entry.lastActivityAt = time.Now()

	if entry.flusher != nil {
		entry.flusher.Flush()
	}

	return nil
}

func (h *ResponseHandler) SendStreamingChunk(requestID string, chunk []byte, isLast bool) error {
	h.mu.RLock()
	entry, exists := h.streamingChannels[requestID]
	h.mu.RUnlock()

	if !exists || entry == nil {
		return nil
	}

	entry.mu.Lock()
	defer entry.mu.Unlock()

	select {
	case <-entry.done:
		return nil
	default:
	}

	if len(chunk) > 0 {
		_, err := entry.w.Write(chunk)
		if err != nil {
			if isClientDisconnectError(err) {
				select {
				case <-entry.done:
				default:
					close(entry.done)
				}
				h.triggerCancel(requestID)
				return nil
			}
			select {
			case <-entry.done:
			default:
				close(entry.done)
			}
			h.triggerCancel(requestID)
			return nil
		}

		entry.lastActivityAt = time.Now()

		if entry.flusher != nil {
			entry.flusher.Flush()
		}
	}

	if isLast {
		select {
		case <-entry.done:
		default:
			close(entry.done)
		}
	}

	return nil
}

func isClientDisconnectError(err error) bool {
	if err == nil {
		return false
	}

	if netErr, ok := err.(*net.OpError); ok {
		if netErr.Err != nil {
			errStr := netErr.Err.Error()
			if strings.Contains(errStr, "broken pipe") ||
				strings.Contains(errStr, "connection reset") ||
				strings.Contains(errStr, "connection refused") {
				return true
			}
		}
	}

	errStr := err.Error()
	return strings.Contains(errStr, "broken pipe") ||
		strings.Contains(errStr, "connection reset") ||
		strings.Contains(errStr, "use of closed network connection")
}

// triggerCancel invokes and removes the cancel callback for a request.
func (h *ResponseHandler) triggerCancel(requestID string) {
	h.mu.Lock()
	cancel := h.cancelFuncs[requestID]
	if cancel != nil {
		delete(h.cancelFuncs, requestID)
	}
	h.mu.Unlock()

	if cancel != nil {
		go func() {
			cancel()
		}()
	}
}

func (h *ResponseHandler) CleanupResponseChan(requestID string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if entry, exists := h.channels[requestID]; exists {
		close(entry.ch)
		delete(h.channels, requestID)
	}
}

func (h *ResponseHandler) CleanupStreamingResponse(requestID string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if entry, exists := h.streamingChannels[requestID]; exists {
		select {
		case <-entry.done:
		default:
			close(entry.done)
		}
		delete(h.streamingChannels, requestID)
	}
}

// CleanupCancelFunc removes a registered cancel callback.
func (h *ResponseHandler) CleanupCancelFunc(requestID string) {
	h.mu.Lock()
	delete(h.cancelFuncs, requestID)
	h.mu.Unlock()
}

func (h *ResponseHandler) GetPendingCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.channels) + len(h.streamingChannels)
}

func (h *ResponseHandler) cleanupLoop() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			h.cleanupExpiredChannels()
		case <-h.stopCh:
			return
		}
	}
}

func (h *ResponseHandler) cleanupExpiredChannels() {
	now := time.Now()
	timeout := 5 * time.Minute
	streamingTimeout := 5 * time.Minute

	h.mu.Lock()
	defer h.mu.Unlock()

	expiredCount := 0
	cancelList := make([]string, 0)
	for requestID, entry := range h.channels {
		if now.Sub(entry.createdAt) > timeout {
			close(entry.ch)
			delete(h.channels, requestID)
			expiredCount++
		}
	}

	for requestID, entry := range h.streamingChannels {
		if now.Sub(entry.lastActivityAt) > streamingTimeout {
			select {
			case <-entry.done:
			default:
				close(entry.done)
			}
			delete(h.streamingChannels, requestID)
			cancelList = append(cancelList, requestID)
			expiredCount++
		}
	}

	for _, requestID := range cancelList {
		if cancel := h.cancelFuncs[requestID]; cancel != nil {
			delete(h.cancelFuncs, requestID)
			go cancel()
		}
	}

	if expiredCount > 0 {
		h.logger.Debug("Cleaned up expired response channels",
			zap.Int("count", expiredCount),
			zap.Int("remaining", len(h.channels)+len(h.streamingChannels)),
		)
	}
}

func (h *ResponseHandler) Close() {
	close(h.stopCh)

	h.mu.Lock()
	defer h.mu.Unlock()

	for _, entry := range h.channels {
		close(entry.ch)
	}
	h.channels = make(map[string]*responseChanEntry)

	for _, entry := range h.streamingChannels {
		select {
		case <-entry.done:
		default:
			close(entry.done)
		}
	}
	h.streamingChannels = make(map[string]*streamingResponseEntry)

	for _, cancel := range h.cancelFuncs {
		cancel()
	}
	h.cancelFuncs = make(map[string]func())
}
