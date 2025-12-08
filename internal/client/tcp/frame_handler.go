package tcp

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"drip/internal/shared/pool"
	"drip/internal/shared/protocol"

	"go.uber.org/zap"
)

// FrameHandler handles data frames and forwards to local service
type FrameHandler struct {
	conn              net.Conn
	frameWriter       *protocol.FrameWriter
	localHost         string
	localPort         int
	logger            *zap.Logger
	streams           map[string]*Stream
	streamMu          sync.RWMutex
	streamingRequests map[string]*StreamingRequest
	streamingReqMu    sync.RWMutex
	responseCancels   map[string]context.CancelFunc
	responseCancelMu  sync.RWMutex
	tunnelType        protocol.TunnelType
	httpClient        *http.Client
	stats             *TrafficStats
	isClosedCheck     func() bool
	bufferPool        *pool.BufferPool
	headerPool        *pool.HeaderPool
}

// Stream represents a single request/response stream
type Stream struct {
	ID         string
	LocalConn  net.Conn
	ResponseCh chan []byte
	Done       chan struct{}
	closed     bool
	mu         sync.Mutex
}

// StreamingRequest represents a streaming upload request in progress
type StreamingRequest struct {
	RequestID  string
	Writer     *io.PipeWriter
	Done       chan struct{}
	chunkQueue chan *chunkData
	closed     bool
	mu         sync.Mutex
}

type chunkData struct {
	data   []byte
	isLast bool
}

func NewFrameHandler(conn net.Conn, frameWriter *protocol.FrameWriter, localHost string, localPort int, tunnelType protocol.TunnelType, logger *zap.Logger, isClosedCheck func() bool, bufferPool *pool.BufferPool) *FrameHandler {
	var tlsConfig *tls.Config
	if tunnelType == protocol.TunnelTypeHTTPS {
		tlsConfig = &tls.Config{
			InsecureSkipVerify: true,
		}
	}

	return &FrameHandler{
		conn:              conn,
		frameWriter:       frameWriter,
		localHost:         localHost,
		localPort:         localPort,
		logger:            logger,
		streams:           make(map[string]*Stream),
		streamingRequests: make(map[string]*StreamingRequest),
		responseCancels:   make(map[string]context.CancelFunc),
		tunnelType:        tunnelType,
		stats:             NewTrafficStats(),
		isClosedCheck:     isClosedCheck,
		bufferPool:        bufferPool,
		headerPool:        pool.NewHeaderPool(),
		httpClient: &http.Client{
			// No overall timeout - streaming responses can take arbitrary time
			Transport: &http.Transport{
				MaxIdleConns:          2000,              // Increased from 1000 for better connection reuse
				MaxIdleConnsPerHost:   1000,              // Increased from 500 for high concurrency
				MaxConnsPerHost:       0,                 // Unlimited connections per host
				IdleConnTimeout:       180 * time.Second, // Keep connections alive for reuse
				DisableCompression:    true,              // Disable compression for better CPU efficiency
				DisableKeepAlives:     false,             // Enable keep-alive for connection reuse
				TLSHandshakeTimeout:   5 * time.Second,   // Reduced from 10s for faster failure detection
				TLSClientConfig:       tlsConfig,
				ResponseHeaderTimeout: 15 * time.Second,       // Reduced from 30s for faster timeout
				ExpectContinueTimeout: 500 * time.Millisecond, // Reduced from 1s for better responsiveness
				WriteBufferSize:       32 * 1024,              // 32KB write buffer
				ReadBufferSize:        32 * 1024,              // 32KB read buffer
				DialContext: (&net.Dialer{
					Timeout:   3 * time.Second,  // Reduced from 5s for faster connection attempts
					KeepAlive: 30 * time.Second, // Keep TCP keepalive
				}).DialContext,
			},
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func (h *FrameHandler) HandleDataFrame(frame *protocol.Frame) error {
	h.stats.AddBytesIn(int64(len(frame.Payload)))
	h.stats.AddRequest()

	header, data, err := protocol.DecodeDataPayload(frame.Payload)
	if err != nil {
		return fmt.Errorf("failed to decode data payload: %w", err)
	}

	if header.Type == protocol.DataTypeHTTPRequest {
		return h.handleHTTPFrame(header, data)
	}

	if header.Type == protocol.DataTypeHTTPRequestHead || header.Type == protocol.DataTypeHTTPHead {
		return h.handleHTTPRequestHead(header, data)
	}

	if header.Type == protocol.DataTypeHTTPRequestBodyChunk || header.Type == protocol.DataTypeHTTPBodyChunk {
		return h.handleHTTPRequestBodyChunk(header, data)
	}

	if header.Type == protocol.DataTypeClose {
		cancelID := header.RequestID
		if cancelID == "" {
			cancelID = header.StreamID
		}
		h.cancelResponse(cancelID)
		h.closeStream(header.StreamID)
		return nil
	}

	stream, err := h.getOrCreateStream(header.StreamID)
	if err != nil {
		return fmt.Errorf("failed to get stream: %w", err)
	}

	h.forwardToLocal(stream, data)

	return nil
}

func (h *FrameHandler) getOrCreateStream(streamID string) (*Stream, error) {
	h.streamMu.Lock()
	defer h.streamMu.Unlock()

	if stream, ok := h.streams[streamID]; ok {
		return stream, nil
	}

	localAddr := net.JoinHostPort(h.localHost, fmt.Sprintf("%d", h.localPort))
	localConn, err := net.DialTimeout("tcp", localAddr, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to local service: %w", err)
	}

	stream := &Stream{
		ID:         streamID,
		LocalConn:  localConn,
		ResponseCh: make(chan []byte, 10),
		Done:       make(chan struct{}),
	}

	h.streams[streamID] = stream

	go h.handleLocalResponse(stream)

	return stream, nil
}

func (h *FrameHandler) forwardToLocal(stream *Stream, data []byte) {
	// Check if stream is closed using mutex
	stream.mu.Lock()
	if stream.closed {
		stream.mu.Unlock()
		return
	}
	stream.mu.Unlock()

	// Double check with Done channel
	select {
	case <-stream.Done:
		// Stream already closed, ignore data
		return
	default:
	}

	if _, err := stream.LocalConn.Write(data); err != nil {
		// Only log at debug level since connection close is often expected
		h.logger.Debug("Failed to write to local service",
			zap.String("stream_id", stream.ID),
			zap.Error(err),
		)
		h.closeStream(stream.ID)
	}
}

func (h *FrameHandler) handleLocalResponse(stream *Stream) {
	defer h.closeStream(stream.ID)

	bufPtr := h.bufferPool.Get(pool.SizeMedium)
	defer h.bufferPool.Put(bufPtr)
	buf := (*bufPtr)[:pool.SizeMedium]

	for {
		// Check if stream is closed before reading
		stream.mu.Lock()
		closed := stream.closed
		stream.mu.Unlock()
		if closed {
			break
		}

		n, err := stream.LocalConn.Read(buf)

		if n > 0 {
			if h.isClosedCheck != nil && h.isClosedCheck() {
				break
			}

			// Check again after read
			stream.mu.Lock()
			closed = stream.closed
			stream.mu.Unlock()
			if closed {
				break
			}

			header := protocol.DataHeader{
				StreamID: stream.ID,
				Type:     protocol.DataTypeResponse,
				IsLast:   false,
			}

			payload, poolBuffer, err := protocol.EncodeDataPayloadPooled(header, buf[:n])
			if err != nil {
				h.logger.Debug("Encode payload failed", zap.Error(err))
				break
			}

			dataFrame := protocol.NewFramePooled(protocol.FrameTypeData, payload, poolBuffer)
			err = h.frameWriter.WriteFrame(dataFrame)
			if err != nil {
				h.logger.Debug("Send frame failed", zap.Error(err))
				break
			}

			h.stats.AddBytesOut(int64(len(payload)))
		}

		if err != nil {
			break
		}
	}
}

func (h *FrameHandler) handleHTTPFrame(header protocol.DataHeader, payload []byte) error {
	if h.tunnelType != protocol.TunnelTypeHTTP && h.tunnelType != protocol.TunnelTypeHTTPS {
		return nil
	}

	httpReq, err := protocol.DecodeHTTPRequest(payload)
	if err != nil {
		return fmt.Errorf("failed to decode HTTP request: %w", err)
	}

	targetURL := httpReq.URL
	if !strings.HasPrefix(targetURL, "http://") && !strings.HasPrefix(targetURL, "https://") {
		scheme := "http"
		if h.tunnelType == protocol.TunnelTypeHTTPS {
			scheme = "https"
		}
		targetURL = fmt.Sprintf("%s://%s:%d%s", scheme, h.localHost, h.localPort, targetURL)
	}

	req, err := http.NewRequest(httpReq.Method, targetURL, bytes.NewReader(httpReq.Body))
	if err != nil {
		return h.sendHTTPError(header.StreamID, header.RequestID, http.StatusBadGateway, fmt.Sprintf("build request: %v", err))
	}

	origHost := ""
	for key, values := range httpReq.Headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	if host := req.Header.Get("Host"); host != "" {
		origHost = host
	}

	isLocalTarget := h.isLocalAddress(h.localHost)

	if isLocalTarget {
		if origHost != "" {
			req.Host = origHost
			req.Header.Set("Host", origHost)
		} else {
			localHostPort := fmt.Sprintf("%s:%d", h.localHost, h.localPort)
			req.Host = localHostPort
			req.Header.Set("Host", localHostPort)
		}
		if origHost != "" {
			req.Header.Set("X-Forwarded-Host", origHost)
		}
	} else {
		targetHost := h.localHost
		if h.localPort != 443 && h.localPort != 80 {
			targetHost = fmt.Sprintf("%s:%d", h.localHost, h.localPort)
		}
		req.Host = targetHost
		req.Header.Set("Host", targetHost)
		if origHost != "" {
			req.Header.Set("X-Forwarded-Host", origHost)
		}
	}
	req.Header.Set("X-Forwarded-Proto", "https")

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return h.sendHTTPError(header.StreamID, header.RequestID, http.StatusBadGateway, fmt.Sprintf("local request failed: %v", err))
	}
	defer resp.Body.Close()

	// Threshold for switching from buffered to streaming mode
	const bufferThreshold int64 = 1 * 1024 * 1024 // 1MB

	// If Content-Length is known and large, use streaming directly
	if resp.ContentLength > bufferThreshold {
		return h.streamHTTPResponse(header.StreamID, header.RequestID, resp)
	}

	// For small or unknown size: try buffered first, switch to streaming if too large
	return h.adaptiveHTTPResponse(header.StreamID, header.RequestID, resp, bufferThreshold)
}

// adaptiveHTTPResponse tries buffered mode first, switches to streaming if data exceeds threshold
func (h *FrameHandler) adaptiveHTTPResponse(streamID, requestID string, resp *http.Response, threshold int64) error {
	if h.isClosedCheck != nil && h.isClosedCheck() {
		return nil
	}

	// Buffer for initial read
	buffer := make([]byte, 0, threshold)
	tempBufPtr := h.bufferPool.Get(pool.SizeMedium)
	defer h.bufferPool.Put(tempBufPtr)
	tempBuf := (*tempBufPtr)[:pool.SizeMedium]

	var totalRead int64
	var hitThreshold bool

	// Try to read up to threshold
	for totalRead < threshold {
		n, err := resp.Body.Read(tempBuf)
		if n > 0 {
			buffer = append(buffer, tempBuf[:n]...)
			totalRead += int64(n)
		}
		if err == io.EOF {
			// Response completed within threshold - use buffered mode
			break
		}
		if err != nil {
			return h.sendHTTPError(streamID, requestID, http.StatusBadGateway, fmt.Sprintf("read response: %v", err))
		}
		if totalRead >= threshold {
			hitThreshold = true
			break
		}
	}

	if !hitThreshold {
		// Small response - send as buffered
		// Clean response headers - remove hop-by-hop headers that are invalid after proxying
		cleanedHeaders := h.cleanResponseHeaders(resp.Header)

		httpResp := protocol.HTTPResponse{
			StatusCode: resp.StatusCode,
			Status:     resp.Status,
			Headers:    cleanedHeaders,
			Body:       buffer,
		}
		return h.sendHTTPResponse(streamID, requestID, &httpResp)
	}

	// Large response - switch to streaming mode
	// Clean response headers - remove hop-by-hop headers that are invalid after proxying
	cleanedHeaders := h.cleanResponseHeaders(resp.Header)

	cancelID := requestID
	if cancelID == "" {
		cancelID = streamID
	}

	h.registerResponseCancel(cancelID, func() {
		resp.Body.Close()
	})
	defer h.unregisterResponseCancel(cancelID)

	// First send headers
	httpHead := protocol.HTTPResponseHead{
		StatusCode:    resp.StatusCode,
		Status:        resp.Status,
		Headers:       cleanedHeaders,
		ContentLength: resp.ContentLength, // -1 if unknown
	}

	headBytes, err := protocol.EncodeHTTPResponseHead(&httpHead)
	if err != nil {
		return fmt.Errorf("encode http head: %w", err)
	}

	headHeader := protocol.DataHeader{
		StreamID:  streamID,
		RequestID: requestID,
		Type:      protocol.DataTypeHTTPHead,
		IsLast:    false,
	}

	headPayload, headPoolBuffer, err := protocol.EncodeDataPayloadPooled(headHeader, headBytes)
	if err != nil {
		return fmt.Errorf("encode head payload: %w", err)
	}

	headFrame := protocol.NewFramePooled(protocol.FrameTypeData, headPayload, headPoolBuffer)
	if err := h.frameWriter.WriteFrame(headFrame); err != nil {
		return err
	}
	h.frameWriter.Flush()
	h.stats.AddBytesOut(int64(len(headPayload)))

	// Send buffered data as first chunk
	if len(buffer) > 0 {
		chunkHeader := protocol.DataHeader{
			StreamID:  streamID,
			RequestID: requestID,
			Type:      protocol.DataTypeHTTPBodyChunk,
			IsLast:    false,
		}

		chunkPayload, chunkPoolBuffer, err := protocol.EncodeDataPayloadPooled(chunkHeader, buffer)
		if err != nil {
			return fmt.Errorf("encode chunk payload: %w", err)
		}

		chunkFrame := protocol.NewFramePooled(protocol.FrameTypeData, chunkPayload, chunkPoolBuffer)
		if err := h.frameWriter.WriteFrame(chunkFrame); err != nil {
			return err
		}
		h.stats.AddBytesOut(int64(len(chunkPayload)))
	}

	// Clear buffer to free memory
	buffer = nil

	// Continue streaming remaining data
	bufPtr := h.bufferPool.Get(pool.SizeMedium)
	defer h.bufferPool.Put(bufPtr)
	buf := (*bufPtr)[:pool.SizeMedium]

	for {
		if h.isClosedCheck != nil && h.isClosedCheck() {
			return nil
		}

		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			isLast := readErr == io.EOF

			chunkHeader := protocol.DataHeader{
				StreamID:  streamID,
				RequestID: requestID,
				Type:      protocol.DataTypeHTTPBodyChunk,
				IsLast:    isLast,
			}

			chunkPayload, chunkPoolBuffer, err := protocol.EncodeDataPayloadPooled(chunkHeader, buf[:n])
			if err != nil {
				return fmt.Errorf("encode chunk payload: %w", err)
			}

			chunkFrame := protocol.NewFramePooled(protocol.FrameTypeData, chunkPayload, chunkPoolBuffer)
			if err := h.frameWriter.WriteFrame(chunkFrame); err != nil {
				return err
			}
			h.stats.AddBytesOut(int64(len(chunkPayload)))
		}

		if readErr == io.EOF {
			if n == 0 {
				// Send final empty chunk
				finalHeader := protocol.DataHeader{
					StreamID:  streamID,
					RequestID: requestID,
					Type:      protocol.DataTypeHTTPBodyChunk,
					IsLast:    true,
				}
				finalPayload, finalPoolBuffer, err := protocol.EncodeDataPayloadPooled(finalHeader, nil)
				if err != nil {
					return fmt.Errorf("encode final payload: %w", err)
				}
				finalFrame := protocol.NewFramePooled(protocol.FrameTypeData, finalPayload, finalPoolBuffer)
				if err := h.frameWriter.WriteFrame(finalFrame); err != nil {
					return err
				}
			}
			h.frameWriter.Flush()
			break
		}
		if readErr != nil {
			// Check for expected errors that indicate connection/body closure
			if errors.Is(readErr, context.Canceled) ||
				errors.Is(readErr, context.DeadlineExceeded) ||
				errors.Is(readErr, http.ErrBodyReadAfterClose) ||
				errors.Is(readErr, net.ErrClosed) ||
				strings.Contains(readErr.Error(), "read on closed response body") {
				return nil
			}
			return fmt.Errorf("read response body: %w", readErr)
		}
	}

	return nil
}

func (h *FrameHandler) sendHTTPError(streamID, requestID string, status int, message string) error {
	headers := h.headerPool.Get()
	headers.Set("Content-Type", "text/plain")

	httpResp := protocol.HTTPResponse{
		StatusCode: status,
		Status:     http.StatusText(status),
		Headers:    headers,
		Body:       []byte(message),
	}

	err := h.sendHTTPResponse(streamID, requestID, &httpResp)

	h.headerPool.Put(headers)

	return err
}

// streamHTTPResponse streams HTTP response using zero-copy approach
// First sends headers, then streams body chunks
func (h *FrameHandler) streamHTTPResponse(streamID, requestID string, resp *http.Response) error {
	if h.isClosedCheck != nil && h.isClosedCheck() {
		return nil
	}

	cancelID := requestID
	if cancelID == "" {
		cancelID = streamID
	}

	h.registerResponseCancel(cancelID, func() {
		resp.Body.Close()
	})
	defer h.unregisterResponseCancel(cancelID)

	// Clean response headers - remove hop-by-hop headers that are invalid after proxying
	cleanedHeaders := h.cleanResponseHeaders(resp.Header)

	// Send HTTP headers first
	contentLength := resp.ContentLength // -1 if unknown
	httpHead := protocol.HTTPResponseHead{
		StatusCode:    resp.StatusCode,
		Status:        resp.Status,
		Headers:       cleanedHeaders,
		ContentLength: contentLength,
	}

	headBytes, err := protocol.EncodeHTTPResponseHead(&httpHead)
	if err != nil {
		return fmt.Errorf("encode http head: %w", err)
	}

	headHeader := protocol.DataHeader{
		StreamID:  streamID,
		RequestID: requestID,
		Type:      protocol.DataTypeHTTPHead,
		IsLast:    false,
	}

	headPayload, headPoolBuffer, err := protocol.EncodeDataPayloadPooled(headHeader, headBytes)
	if err != nil {
		return fmt.Errorf("encode head payload: %w", err)
	}

	headFrame := protocol.NewFramePooled(protocol.FrameTypeData, headPayload, headPoolBuffer)
	if err := h.frameWriter.WriteFrame(headFrame); err != nil {
		return err
	}
	h.frameWriter.Flush()
	h.stats.AddBytesOut(int64(len(headPayload)))

	// Stream body chunks - zero copy using buffer pool
	bufPtr := h.bufferPool.Get(pool.SizeMedium)
	defer h.bufferPool.Put(bufPtr)
	buf := (*bufPtr)[:pool.SizeMedium]

	for {
		if h.isClosedCheck != nil && h.isClosedCheck() {
			return nil
		}

		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			isLast := readErr == io.EOF

			chunkHeader := protocol.DataHeader{
				StreamID:  streamID,
				RequestID: requestID,
				Type:      protocol.DataTypeHTTPBodyChunk,
				IsLast:    isLast,
			}

			chunkPayload, chunkPoolBuffer, err := protocol.EncodeDataPayloadPooled(chunkHeader, buf[:n])
			if err != nil {
				return fmt.Errorf("encode chunk payload: %w", err)
			}

			chunkFrame := protocol.NewFramePooled(protocol.FrameTypeData, chunkPayload, chunkPoolBuffer)
			if err := h.frameWriter.WriteFrame(chunkFrame); err != nil {
				return err
			}
			h.stats.AddBytesOut(int64(len(chunkPayload)))
		}

		if readErr == io.EOF {
			// Send final empty chunk with IsLast=true if we haven't already
			if n == 0 {
				finalHeader := protocol.DataHeader{
					StreamID:  streamID,
					RequestID: requestID,
					Type:      protocol.DataTypeHTTPBodyChunk,
					IsLast:    true,
				}
				finalPayload, finalPoolBuffer, err := protocol.EncodeDataPayloadPooled(finalHeader, nil)
				if err != nil {
					return fmt.Errorf("encode final payload: %w", err)
				}
				finalFrame := protocol.NewFramePooled(protocol.FrameTypeData, finalPayload, finalPoolBuffer)
				if err := h.frameWriter.WriteFrame(finalFrame); err != nil {
					return err
				}
			}
			h.frameWriter.Flush()
			break
		}
		if readErr != nil {
			// Check for expected errors that indicate connection/body closure
			if errors.Is(readErr, context.Canceled) ||
				errors.Is(readErr, context.DeadlineExceeded) ||
				errors.Is(readErr, http.ErrBodyReadAfterClose) ||
				errors.Is(readErr, net.ErrClosed) ||
				strings.Contains(readErr.Error(), "read on closed response body") {
				return nil
			}
			return fmt.Errorf("read response body: %w", readErr)
		}
	}

	return nil
}

func (h *FrameHandler) sendHTTPResponse(streamID, requestID string, resp *protocol.HTTPResponse) error {
	if h.isClosedCheck != nil && h.isClosedCheck() {
		return nil
	}

	header := protocol.DataHeader{
		StreamID:  streamID,
		RequestID: requestID,
		Type:      protocol.DataTypeHTTPResponse,
		IsLast:    true,
	}

	respBytes, err := protocol.EncodeHTTPResponse(resp)
	if err != nil {
		return fmt.Errorf("encode http response: %w", err)
	}

	payload, poolBuffer, err := protocol.EncodeDataPayloadPooled(header, respBytes)
	if err != nil {
		return fmt.Errorf("encode payload: %w", err)
	}

	dataFrame := protocol.NewFramePooled(protocol.FrameTypeData, payload, poolBuffer)

	h.stats.AddBytesOut(int64(len(payload)))

	if err := h.frameWriter.WriteFrame(dataFrame); err != nil {
		return err
	}

	// Flush immediately to ensure the response is sent without batching delay
	h.frameWriter.Flush()
	return nil
}

func (h *FrameHandler) closeStream(streamID string) {
	h.streamMu.Lock()
	stream, ok := h.streams[streamID]
	if !ok {
		h.streamMu.Unlock()
		return
	}

	// Use stream-level mutex to prevent race conditions
	stream.mu.Lock()
	if stream.closed {
		stream.mu.Unlock()
		h.streamMu.Unlock()
		return
	}
	stream.closed = true
	stream.mu.Unlock()

	// Remove from map first to prevent concurrent access
	delete(h.streams, streamID)
	h.streamMu.Unlock()

	// Now safe to close resources without holding the main lock
	if stream.LocalConn != nil {
		stream.LocalConn.Close()
	}

	close(stream.Done)

	if h.isClosedCheck != nil && h.isClosedCheck() {
		return
	}

	header := protocol.DataHeader{
		StreamID:  streamID,
		RequestID: streamID,
		Type:      protocol.DataTypeClose,
		IsLast:    true,
	}

	payload, poolBuffer, err := protocol.EncodeDataPayloadPooled(header, nil)
	if err != nil {
		return
	}

	closeFrame := protocol.NewFramePooled(protocol.FrameTypeData, payload, poolBuffer)

	h.frameWriter.WriteFrame(closeFrame)
}

// Close closes all streams
func (h *FrameHandler) Close() {
	h.streamMu.Lock()
	for streamID, stream := range h.streams {
		stream.mu.Lock()
		if !stream.closed {
			stream.closed = true
			if stream.LocalConn != nil {
				stream.LocalConn.Close()
			}
			close(stream.Done)
		}
		stream.mu.Unlock()
		delete(h.streams, streamID)
	}
	h.streamMu.Unlock()

	h.streamingReqMu.Lock()
	for requestID, streamingReq := range h.streamingRequests {
		h.closeStreamingRequest(requestID, streamingReq)
		if streamingReq.Writer != nil {
			streamingReq.Writer.CloseWithError(fmt.Errorf("tunnel connection closed"))
		}
		delete(h.streamingRequests, requestID)
	}
	h.streamingReqMu.Unlock()
}

// GetStats returns the traffic stats tracker
func (h *FrameHandler) GetStats() *TrafficStats {
	return h.stats
}

func (h *FrameHandler) WarmupConnectionPool(numConnections int) {
	if h.tunnelType != protocol.TunnelTypeHTTP {
		return
	}

	targetURL := fmt.Sprintf("http://%s:%d/", h.localHost, h.localPort)

	var wg sync.WaitGroup
	for i := 0; i < numConnections; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			req, err := http.NewRequest("HEAD", targetURL, nil)
			if err != nil {
				return
			}

			resp, err := h.httpClient.Do(req)
			if err != nil {
				return
			}
			defer resp.Body.Close()
			io.Copy(io.Discard, resp.Body)
		}()
	}

	wg.Wait()
}

func (h *FrameHandler) isLocalAddress(addr string) bool {
	if addr == "localhost" || addr == "127.0.0.1" || addr == "::1" {
		return true
	}

	if strings.HasPrefix(addr, "192.168.") ||
		strings.HasPrefix(addr, "10.") ||
		strings.HasPrefix(addr, "172.16.") ||
		strings.HasPrefix(addr, "172.17.") ||
		strings.HasPrefix(addr, "172.18.") ||
		strings.HasPrefix(addr, "172.19.") ||
		strings.HasPrefix(addr, "172.20.") ||
		strings.HasPrefix(addr, "172.21.") ||
		strings.HasPrefix(addr, "172.22.") ||
		strings.HasPrefix(addr, "172.23.") ||
		strings.HasPrefix(addr, "172.24.") ||
		strings.HasPrefix(addr, "172.25.") ||
		strings.HasPrefix(addr, "172.26.") ||
		strings.HasPrefix(addr, "172.27.") ||
		strings.HasPrefix(addr, "172.28.") ||
		strings.HasPrefix(addr, "172.29.") ||
		strings.HasPrefix(addr, "172.30.") ||
		strings.HasPrefix(addr, "172.31.") {
		return true
	}

	return false
}

// cleanResponseHeaders removes hop-by-hop headers that should not be forwarded
// Go's http.Client automatically handles chunked encoding, so we need to remove
// the Transfer-Encoding header to avoid sending decoded body with chunked header
func (h *FrameHandler) cleanResponseHeaders(headers http.Header) http.Header {
	cleaned := make(http.Header)

	// List of hop-by-hop headers to remove (RFC 2616)
	hopByHopHeaders := map[string]bool{
		"Connection":          true,
		"Keep-Alive":          true,
		"Proxy-Authenticate":  true,
		"Proxy-Authorization": true,
		"Te":                  true,
		"Trailers":            true,
		"Transfer-Encoding":   true,
		"Upgrade":             true,
		"Proxy-Connection":    true,
	}

	for key, values := range headers {
		canonicalKey := http.CanonicalHeaderKey(key)

		if hopByHopHeaders[canonicalKey] {
			continue
		}

		// Also check if this header is listed in Connection header
		connectionHeaders := headers.Get("Connection")
		if connectionHeaders != "" {
			tokens := strings.Split(connectionHeaders, ",")
			skip := false
			for _, token := range tokens {
				if strings.TrimSpace(token) == key {
					skip = true
					break
				}
			}
			if skip {
				continue
			}
		}

		for _, value := range values {
			cleaned.Add(key, value)
		}
	}

	return cleaned
}

func (h *FrameHandler) handleHTTPRequestHead(header protocol.DataHeader, payload []byte) error {
	httpReqHead, err := protocol.DecodeHTTPRequestHead(payload)
	if err != nil {
		return fmt.Errorf("failed to decode HTTP request head: %w", err)
	}

	requestID := header.RequestID
	if requestID == "" {
		requestID = header.StreamID
	}

	targetURL := httpReqHead.URL
	if !strings.HasPrefix(targetURL, "http://") && !strings.HasPrefix(targetURL, "https://") {
		scheme := "http"
		if h.tunnelType == protocol.TunnelTypeHTTPS {
			scheme = "https"
		}
		targetURL = fmt.Sprintf("%s://%s:%d%s", scheme, h.localHost, h.localPort, targetURL)
	}

	pipeReader, pipeWriter := io.Pipe()

	req, err := http.NewRequest(httpReqHead.Method, targetURL, pipeReader)
	if err != nil {
		pipeWriter.Close()
		return h.sendHTTPError(header.StreamID, requestID, http.StatusBadGateway, fmt.Sprintf("build request: %v", err))
	}

	origHost := ""
	for key, values := range httpReqHead.Headers {
		if key == "Content-Length" {
			continue
		}
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	if host := req.Header.Get("Host"); host != "" {
		origHost = host
	}

	req.ContentLength = -1

	isLocalTarget := h.isLocalAddress(h.localHost)
	if isLocalTarget {
		if origHost != "" {
			req.Host = origHost
			req.Header.Set("Host", origHost)
		} else {
			localHostPort := fmt.Sprintf("%s:%d", h.localHost, h.localPort)
			req.Host = localHostPort
			req.Header.Set("Host", localHostPort)
		}
		if origHost != "" {
			req.Header.Set("X-Forwarded-Host", origHost)
		}
	} else {
		targetHost := h.localHost
		if h.localPort != 443 && h.localPort != 80 {
			targetHost = fmt.Sprintf("%s:%d", h.localHost, h.localPort)
		}
		req.Host = targetHost
		req.Header.Set("Host", targetHost)
		if origHost != "" {
			req.Header.Set("X-Forwarded-Host", origHost)
		}
	}
	req.Header.Set("X-Forwarded-Proto", "https")

	streamingReq := &StreamingRequest{
		RequestID:  requestID,
		Writer:     pipeWriter,
		Done:       make(chan struct{}),
		chunkQueue: make(chan *chunkData, 512), // deeper buffer for bursty body chunks
	}

	h.streamingReqMu.Lock()
	h.streamingRequests[requestID] = streamingReq
	h.streamingReqMu.Unlock()

	go func() {
		defer pipeWriter.Close()

		timeout := time.NewTimer(5 * time.Minute) // Timeout for receiving body chunks
		defer timeout.Stop()

		for {
			select {
			case chunk, ok := <-streamingReq.chunkQueue:
				if !ok || chunk == nil {
					return
				}

				// Reset timeout on each chunk
				if !timeout.Stop() {
					select {
					case <-timeout.C:
					default:
					}
				}
				timeout.Reset(5 * time.Minute)

				if len(chunk.data) > 0 {
					if _, err := pipeWriter.Write(chunk.data); err != nil {
						h.logger.Error("Failed to write to pipe",
							zap.String("request_id", requestID),
							zap.Error(err),
						)
						pipeWriter.CloseWithError(err)
						return
					}
				}

				if chunk.isLast {
					return
				}
			case <-streamingReq.Done:
				return
			case <-timeout.C:
				h.logger.Warn("Timeout waiting for request body chunks",
					zap.String("request_id", requestID),
				)
				pipeWriter.CloseWithError(fmt.Errorf("timeout waiting for body chunks"))
				return
			}
		}
	}()

	go func() {
		defer func() {
			h.closeStreamingRequest(requestID, streamingReq)
			h.streamingReqMu.Lock()
			delete(h.streamingRequests, requestID)
			h.streamingReqMu.Unlock()
		}()

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()

		reqWithCtx := req.WithContext(ctx)

		resp, err := h.httpClient.Do(reqWithCtx)
		if err != nil {
			h.sendHTTPError(header.StreamID, requestID, http.StatusBadGateway, fmt.Sprintf("local request failed: %v", err))
			return
		}
		defer resp.Body.Close()

		const bufferThreshold int64 = 1 * 1024 * 1024
		if resp.ContentLength > bufferThreshold {
			h.streamHTTPResponse(header.StreamID, requestID, resp)
		} else {
			h.adaptiveHTTPResponse(header.StreamID, requestID, resp, bufferThreshold)
		}
	}()

	return nil
}

func (h *FrameHandler) handleHTTPRequestBodyChunk(header protocol.DataHeader, data []byte) error {
	requestID := header.RequestID
	if requestID == "" {
		requestID = header.StreamID
	}

	h.streamingReqMu.RLock()
	streamingReq, exists := h.streamingRequests[requestID]
	h.streamingReqMu.RUnlock()

	if !exists {
		h.logger.Warn("Streaming request not found for body chunk",
			zap.String("request_id", requestID),
		)
		return nil
	}

	streamingReq.mu.Lock()
	if streamingReq.closed {
		streamingReq.mu.Unlock()
		h.logger.Debug("Streaming request already closed",
			zap.String("request_id", requestID),
		)
		return nil
	}
	streamingReq.mu.Unlock()

	chunk := &chunkData{
		data:   make([]byte, len(data)),
		isLast: header.IsLast,
	}
	copy(chunk.data, data)

	select {
	case streamingReq.chunkQueue <- chunk:
	case <-streamingReq.Done:
		h.logger.Debug("Streaming request already closed",
			zap.String("request_id", requestID),
		)
		return nil
	}

	if header.IsLast {
		h.closeStreamingRequest(requestID, streamingReq)
		h.streamingReqMu.Lock()
		delete(h.streamingRequests, requestID)
		h.streamingReqMu.Unlock()
	}

	return nil
}

// closeStreamingRequest marks a streaming request closed and signals goroutines.
func (h *FrameHandler) closeStreamingRequest(requestID string, streamingReq *StreamingRequest) {
	streamingReq.mu.Lock()
	if streamingReq.closed {
		streamingReq.mu.Unlock()
		return
	}
	streamingReq.closed = true
	close(streamingReq.Done)
	streamingReq.mu.Unlock()
}

func (h *FrameHandler) registerResponseCancel(id string, cancel context.CancelFunc) {
	if cancel == nil {
		return
	}

	h.responseCancelMu.Lock()
	h.responseCancels[id] = cancel
	h.responseCancelMu.Unlock()
}

func (h *FrameHandler) cancelResponse(id string) {
	h.responseCancelMu.Lock()
	cancel := h.responseCancels[id]
	if cancel != nil {
		delete(h.responseCancels, id)
	}
	h.responseCancelMu.Unlock()

	if cancel != nil {
		cancel()
	}
}

func (h *FrameHandler) unregisterResponseCancel(id string) {
	h.responseCancelMu.Lock()
	delete(h.responseCancels, id)
	h.responseCancelMu.Unlock()
}
