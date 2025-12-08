package tcp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	json "github.com/goccy/go-json"

	"drip/internal/server/tunnel"
	"drip/internal/shared/constants"
	"drip/internal/shared/protocol"

	"go.uber.org/zap"
)

// Connection represents a client TCP connection
type Connection struct {
	conn          net.Conn
	authToken     string
	manager       *tunnel.Manager
	logger        *zap.Logger
	subdomain     string
	port          int
	domain        string
	publicPort    int
	portAlloc     *PortAllocator
	tunnelConn    *tunnel.Connection
	proxy         *TunnelProxy
	stopCh        chan struct{}
	once          sync.Once
	lastHeartbeat time.Time
	mu            sync.RWMutex
	frameWriter   *protocol.FrameWriter
	httpHandler   http.Handler
	responseChans HTTPResponseHandler
	tunnelType    protocol.TunnelType // Track tunnel type
	ctx           context.Context
	cancel        context.CancelFunc
}

// HTTPResponseHandler interface for response channel operations
type HTTPResponseHandler interface {
	CreateResponseChan(requestID string) chan *protocol.HTTPResponse
	GetResponseChan(requestID string) <-chan *protocol.HTTPResponse
	CleanupResponseChan(requestID string)
	SendResponse(requestID string, resp *protocol.HTTPResponse)
	// Streaming response methods
	SendStreamingHead(requestID string, head *protocol.HTTPResponseHead) error
	SendStreamingChunk(requestID string, chunk []byte, isLast bool) error
}

// NewConnection creates a new connection handler
func NewConnection(conn net.Conn, authToken string, manager *tunnel.Manager, logger *zap.Logger, portAlloc *PortAllocator, domain string, publicPort int, httpHandler http.Handler, responseChans HTTPResponseHandler) *Connection {
	ctx, cancel := context.WithCancel(context.Background())
	return &Connection{
		conn:          conn,
		authToken:     authToken,
		manager:       manager,
		logger:        logger,
		portAlloc:     portAlloc,
		domain:        domain,
		publicPort:    publicPort,
		httpHandler:   httpHandler,
		responseChans: responseChans,
		stopCh:        make(chan struct{}),
		lastHeartbeat: time.Now(),
		ctx:           ctx,
		cancel:        cancel,
	}
}

// Handle handles the connection lifecycle
func (c *Connection) Handle() error {
	// Register connection for adaptive load tracking
	protocol.RegisterConnection()

	// Ensure cleanup of control connection, proxy, port, and registry on exit.
	defer c.Close()

	// Set initial read timeout for protocol detection
	c.conn.SetReadDeadline(time.Now().Add(30 * time.Second))

	// Use buffered reader to support peeking
	reader := bufio.NewReader(c.conn)

	// Peek first 8 bytes to detect protocol
	peek, err := reader.Peek(8)
	if err != nil {
		return fmt.Errorf("failed to peek connection: %w", err)
	}

	peekStr := string(peek)
	httpMethods := []string{"GET ", "POST", "PUT ", "DELE", "HEAD", "OPTI", "PATC", "CONN", "TRAC"}
	isHTTP := false
	for _, method := range httpMethods {
		if strings.HasPrefix(peekStr, method) {
			isHTTP = true
			break
		}
	}

	if isHTTP {
		c.logger.Info("Detected HTTP request on TCP port, handling as HTTP")
		return c.handleHTTPRequest(reader)
	}

	// Continue with drip protocol
	// Wait for registration frame
	frame, err := protocol.ReadFrame(reader)
	if err != nil {
		return fmt.Errorf("failed to read registration frame: %w", err)
	}
	defer frame.Release() // Return pool buffer when done

	if frame.Type != protocol.FrameTypeRegister {
		return fmt.Errorf("expected register frame, got %s", frame.Type)
	}

	var req protocol.RegisterRequest
	if err := json.Unmarshal(frame.Payload, &req); err != nil {
		return fmt.Errorf("failed to parse registration request: %w", err)
	}

	c.tunnelType = req.TunnelType

	if c.authToken != "" && req.Token != c.authToken {
		c.sendError("authentication_failed", "Invalid authentication token")
		return fmt.Errorf("authentication failed")
	}

	// Allocate TCP port only for TCP tunnels
	if req.TunnelType == protocol.TunnelTypeTCP {
		if c.portAlloc == nil {
			return fmt.Errorf("port allocator not configured")
		}

		port, err := c.portAlloc.Allocate()
		if err != nil {
			c.sendError("port_allocation_failed", err.Error())
			return fmt.Errorf("failed to allocate port: %w", err)
		}
		c.port = port

		// For TCP tunnels, prefer deterministic subdomain tied to port when not provided by client.
		if req.CustomSubdomain == "" {
			req.CustomSubdomain = fmt.Sprintf("tcp-%d", port)
		}
	}

	subdomain, err := c.manager.Register(nil, req.CustomSubdomain)
	if err != nil {
		c.sendError("registration_failed", err.Error())
		c.portAlloc.Release(c.port)
		c.port = 0
		return fmt.Errorf("tunnel registration failed: %w", err)
	}

	c.subdomain = subdomain

	tunnelConn, ok := c.manager.Get(subdomain)
	if !ok {
		return fmt.Errorf("failed to get registered tunnel")
	}
	c.tunnelConn = tunnelConn

	// Store TCP connection reference and metadata for HTTP proxy routing
	c.tunnelConn.Conn = nil // We're using TCP, not WebSocket
	c.tunnelConn.SetTransport(c, req.TunnelType)
	c.tunnelConn.SetTunnelType(req.TunnelType)
	c.tunnelType = req.TunnelType

	c.logger.Info("Tunnel registered",
		zap.String("subdomain", subdomain),
		zap.String("tunnel_type", string(req.TunnelType)),
		zap.Int("local_port", req.LocalPort),
		zap.Int("remote_port", c.port),
	)

	// Send registration acknowledgment
	// Generate appropriate URL based on tunnel type
	var tunnelURL string

	if req.TunnelType == protocol.TunnelTypeHTTP || req.TunnelType == protocol.TunnelTypeHTTPS {
		// HTTP/HTTPS tunnels use HTTPS with subdomain
		// Use publicPort for URL generation (configured via --public-port flag)
		if c.publicPort == 443 {
			tunnelURL = fmt.Sprintf("https://%s.%s", subdomain, c.domain)
		} else {
			tunnelURL = fmt.Sprintf("https://%s.%s:%d", subdomain, c.domain, c.publicPort)
		}
	} else {
		// TCP tunnels use tcp:// with port
		tunnelURL = fmt.Sprintf("tcp://%s:%d", c.domain, c.port)
	}

	resp := protocol.RegisterResponse{
		Subdomain: subdomain,
		Port:      c.port,
		URL:       tunnelURL,
		Message:   "Tunnel registered successfully",
	}

	respData, _ := json.Marshal(resp)
	ackFrame := protocol.NewFrame(protocol.FrameTypeRegisterAck, respData)

	// Send registration ack (sync write before frameWriter is created)
	err = protocol.WriteFrame(c.conn, ackFrame)
	if err != nil {
		return fmt.Errorf("failed to send registration ack: %w", err)
	}

	c.frameWriter = protocol.NewFrameWriter(c.conn)

	c.frameWriter.SetWriteErrorHandler(func(err error) {
		c.logger.Error("Write error detected, closing connection", zap.Error(err))
		c.Close()
	})

	c.conn.SetReadDeadline(time.Time{})

	if req.TunnelType == protocol.TunnelTypeTCP {
		c.proxy = NewTunnelProxy(c.port, subdomain, c.conn, c.logger)
		if err := c.proxy.Start(); err != nil {
			return fmt.Errorf("failed to start TCP proxy: %w", err)
		}
	}

	go c.heartbeatChecker()

	return c.handleFrames(reader)
}

func (c *Connection) handleHTTPRequest(reader *bufio.Reader) error {
	if c.httpHandler == nil {
		c.logger.Warn("HTTP request received but no HTTP handler configured")
		response := "HTTP/1.1 503 Service Unavailable\r\n" +
			"Content-Type: text/plain\r\n" +
			"Content-Length: 47\r\n" +
			"\r\n" +
			"HTTP handler not configured for this TCP port\r\n"
		c.conn.Write([]byte(response))
		return fmt.Errorf("HTTP handler not configured")
	}

	// Clear read deadline for HTTP processing
	c.conn.SetReadDeadline(time.Time{})

	// Handle multiple HTTP requests on the same connection (HTTP/1.1 keep-alive)
	for {
		// Set a read deadline for each request to avoid hanging forever
		c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))

		// Parse HTTP request
		req, err := http.ReadRequest(reader)
		if err != nil {
			// EOF or timeout is normal when client closes connection or no more requests
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				c.logger.Debug("Client closed HTTP connection")
				return nil
			}
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				c.logger.Debug("HTTP keep-alive timeout")
				return nil
			}
			// Connection reset by peer is normal - client closed connection abruptly
			errStr := err.Error()
			if errors.Is(err, net.ErrClosed) || strings.Contains(errStr, "use of closed network connection") {
				c.logger.Debug("HTTP connection closed during read", zap.Error(err))
				return nil
			}
			if strings.Contains(errStr, "connection reset by peer") ||
				strings.Contains(errStr, "broken pipe") ||
				strings.Contains(errStr, "connection refused") {
				c.logger.Debug("Client disconnected abruptly", zap.Error(err))
				return nil
			}
			// Check if it looks like garbage data (not a valid HTTP request)
			if strings.Contains(errStr, "malformed HTTP") {
				c.logger.Warn("Received malformed HTTP request, possibly due to pipelined requests or protocol error",
					zap.Error(err),
					zap.String("error_snippet", errStr[:min(len(errStr), 100)]),
				)
				// Close connection on malformed request to prevent further errors
				return nil
			}
			c.logger.Error("Failed to parse HTTP request", zap.Error(err))
			return fmt.Errorf("failed to parse HTTP request: %w", err)
		}

		if c.ctx != nil {
			req = req.WithContext(c.ctx)
		}

		c.logger.Info("Processing HTTP request on TCP port",
			zap.String("method", req.Method),
			zap.String("url", req.URL.String()),
			zap.String("host", req.Host),
		)

		respWriter := &httpResponseWriter{
			conn:   c.conn,
			writer: bufio.NewWriterSize(c.conn, 4096),
			header: make(http.Header),
		}

		c.httpHandler.ServeHTTP(respWriter, req)

		if err := respWriter.writer.Flush(); err != nil {
			c.logger.Debug("Failed to flush HTTP response", zap.Error(err))
		}

		if tcpConn, ok := c.conn.(*net.TCPConn); ok {
			tcpConn.SetNoDelay(true)
			tcpConn.SetNoDelay(false)
		}

		c.logger.Debug("HTTP request processing completed",
			zap.String("method", req.Method),
			zap.String("url", req.URL.String()),
		)

		shouldClose := false
		if req.Close {
			shouldClose = true
		} else if req.ProtoMajor == 1 && req.ProtoMinor == 0 {
			if req.Header.Get("Connection") != "keep-alive" {
				shouldClose = true
			}
		}

		if respWriter.headerWritten && respWriter.header.Get("Connection") == "close" {
			shouldClose = true
		}

		if shouldClose {
			c.logger.Debug("Closing connection as requested by client or server")
			return nil
		}

		// Continue to next request on the same connection
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// handleFrames handles incoming frames
func (c *Connection) handleFrames(reader *bufio.Reader) error {
	for {
		select {
		case <-c.stopCh:
			return nil
		default:
		}

		// Read frame with timeout
		c.conn.SetReadDeadline(time.Now().Add(constants.RequestTimeout))
		frame, err := protocol.ReadFrame(reader)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				c.logger.Warn("Read timeout, connection may be dead")
				return fmt.Errorf("read timeout")
			}
			// EOF is normal when client closes connection gracefully
			if err.Error() == "failed to read frame header: EOF" || err.Error() == "EOF" {
				c.logger.Info("Client disconnected")
				return nil
			}
			// Check if connection was closed (during shutdown)
			select {
			case <-c.stopCh:
				// Connection was closed intentionally, don't log as error
				c.logger.Debug("Connection closed during shutdown")
				return nil
			default:
				return fmt.Errorf("failed to read frame: %w", err)
			}
		}

		// Handle frame based on type
		switch frame.Type {
		case protocol.FrameTypeHeartbeat:
			c.handleHeartbeat()
			frame.Release()

		case protocol.FrameTypeData:
			// Data frame from client (response to forwarded request)
			c.handleDataFrame(frame)
			frame.Release() // Release after processing

		case protocol.FrameTypeClose:
			frame.Release()
			c.logger.Info("Client requested close")
			return nil

		default:
			frame.Release()
			c.logger.Warn("Unexpected frame type",
				zap.String("type", frame.Type.String()),
			)
		}
	}
}

// handleHeartbeat handles heartbeat frame
func (c *Connection) handleHeartbeat() {
	c.mu.Lock()
	c.lastHeartbeat = time.Now()
	c.mu.Unlock()

	// Send heartbeat ack
	ackFrame := protocol.NewFrame(protocol.FrameTypeHeartbeatAck, nil)

	err := c.frameWriter.WriteFrame(ackFrame)
	if err != nil {
		c.logger.Error("Failed to send heartbeat ack", zap.Error(err))
	}
}

// handleDataFrame handles data frame (response from client)
func (c *Connection) handleDataFrame(frame *protocol.Frame) {
	// Decode payload (auto-detects protocol version)
	header, data, err := protocol.DecodeDataPayload(frame.Payload)
	if err != nil {
		c.logger.Error("Failed to decode data payload",
			zap.Error(err),
		)
		return
	}

	c.logger.Debug("Received data frame",
		zap.String("stream_id", header.StreamID),
		zap.String("type", header.Type.String()),
		zap.Int("data_size", len(data)),
	)

	switch header.Type {
	case protocol.DataTypeResponse:
		// TCP tunnel response, forward to proxy
		if c.proxy != nil {
			if err := c.proxy.HandleResponse(header.StreamID, data); err != nil {
				c.logger.Error("Failed to handle response",
					zap.String("stream_id", header.StreamID),
					zap.Error(err),
				)
			}
		}
	case protocol.DataTypeHTTPResponse:
		if c.responseChans == nil {
			c.logger.Warn("No response channel handler for HTTP response",
				zap.String("stream_id", header.StreamID),
			)
			return
		}

		// Decode HTTP response (auto-detects JSON vs msgpack)
		httpResp, err := protocol.DecodeHTTPResponse(data)
		if err != nil {
			c.logger.Error("Failed to decode HTTP response",
				zap.String("stream_id", header.StreamID),
				zap.Error(err),
			)
			return
		}

		// Route by request ID when provided to keep request/response aligned.
		reqID := header.RequestID
		if reqID == "" {
			reqID = header.StreamID
		}

		c.responseChans.SendResponse(reqID, httpResp)
	case protocol.DataTypeHTTPHead:
		// Streaming HTTP response headers
		if c.responseChans == nil {
			c.logger.Warn("No response handler for streaming HTTP head",
				zap.String("stream_id", header.StreamID),
			)
			return
		}

		httpHead, err := protocol.DecodeHTTPResponseHead(data)
		if err != nil {
			c.logger.Error("Failed to decode HTTP response head",
				zap.String("stream_id", header.StreamID),
				zap.Error(err),
			)
			return
		}

		reqID := header.RequestID
		if reqID == "" {
			reqID = header.StreamID
		}

		if err := c.responseChans.SendStreamingHead(reqID, httpHead); err != nil {
			c.logger.Error("Failed to send streaming head",
				zap.String("request_id", reqID),
				zap.Error(err),
			)
		}
	case protocol.DataTypeHTTPBodyChunk:
		// Streaming HTTP response body chunk
		if c.responseChans == nil {
			c.logger.Warn("No response handler for streaming HTTP chunk",
				zap.String("stream_id", header.StreamID),
			)
			return
		}

		reqID := header.RequestID
		if reqID == "" {
			reqID = header.StreamID
		}

		if err := c.responseChans.SendStreamingChunk(reqID, data, header.IsLast); err != nil {
			c.logger.Error("Failed to send streaming chunk",
				zap.String("request_id", reqID),
				zap.Error(err),
			)
		}
	case protocol.DataTypeClose:
		// Client is closing the stream
		if c.proxy != nil {
			c.proxy.CloseStream(header.StreamID)
		}
	default:
		c.logger.Warn("Unknown data frame type",
			zap.String("type", header.Type.String()),
			zap.String("stream_id", header.StreamID),
		)
	}
}

// heartbeatChecker checks for heartbeat timeout
func (c *Connection) heartbeatChecker() {
	ticker := time.NewTicker(constants.HeartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
			c.mu.RLock()
			lastHB := c.lastHeartbeat
			c.mu.RUnlock()

			if time.Since(lastHB) > constants.HeartbeatTimeout {
				c.logger.Warn("Heartbeat timeout",
					zap.String("subdomain", c.subdomain),
					zap.Duration("last_heartbeat", time.Since(lastHB)),
				)
				c.Close()
				return
			}
		}
	}
}

func (c *Connection) SendFrame(frame *protocol.Frame) error {
	if c.frameWriter == nil {
		return protocol.WriteFrame(c.conn, frame)
	}
	return c.frameWriter.WriteFrame(frame)
}

func (c *Connection) sendError(code, message string) {
	errMsg := protocol.ErrorMessage{
		Code:    code,
		Message: message,
	}
	data, _ := json.Marshal(errMsg)
	errFrame := protocol.NewFrame(protocol.FrameTypeError, data)

	if c.frameWriter == nil {
		protocol.WriteFrame(c.conn, errFrame)
	} else {
		c.frameWriter.WriteFrame(errFrame)
	}
}

func (c *Connection) Close() {
	c.once.Do(func() {
		protocol.UnregisterConnection()

		close(c.stopCh)

		if c.cancel != nil {
			c.cancel()
		}

		if c.frameWriter != nil {
			c.frameWriter.Flush()
			c.frameWriter.Close()
		}

		if c.proxy != nil {
			c.proxy.Stop()
		}

		c.conn.Close()

		if c.port > 0 && c.portAlloc != nil {
			c.portAlloc.Release(c.port)
		}

		if c.subdomain != "" {
			c.manager.Unregister(c.subdomain)
		}

		c.logger.Info("Connection closed",
			zap.String("subdomain", c.subdomain),
		)
	})
}

// GetSubdomain returns the assigned subdomain
func (c *Connection) GetSubdomain() string {
	return c.subdomain
}

// httpResponseWriter implements http.ResponseWriter for writing to a net.Conn
type httpResponseWriter struct {
	conn          net.Conn
	writer        *bufio.Writer // Buffered writer for efficient I/O
	header        http.Header
	statusCode    int
	headerWritten bool
}

func (w *httpResponseWriter) Header() http.Header {
	return w.header
}

func (w *httpResponseWriter) WriteHeader(statusCode int) {
	if w.headerWritten {
		return
	}
	w.statusCode = statusCode
	w.headerWritten = true

	statusText := http.StatusText(statusCode)
	if statusText == "" {
		statusText = "Unknown"
	}

	w.writer.WriteString("HTTP/1.1 ")
	w.writer.WriteString(fmt.Sprintf("%d", statusCode))
	w.writer.WriteByte(' ')
	w.writer.WriteString(statusText)
	w.writer.WriteString("\r\n")

	for key, values := range w.header {
		for _, value := range values {
			w.writer.WriteString(key)
			w.writer.WriteString(": ")
			w.writer.WriteString(value)
			w.writer.WriteString("\r\n")
		}
	}

	w.writer.WriteString("\r\n")
}

func (w *httpResponseWriter) Write(data []byte) (int, error) {
	if !w.headerWritten {
		w.WriteHeader(http.StatusOK)
	}
	return w.writer.Write(data)
}
