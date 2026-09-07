package tcp

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"drip/internal/shared/utils"

	"go.uber.org/zap"
)

// httpRespWriterPool reuses bufio.Writer instances for raw HTTP response writing
var httpRespWriterPool = sync.Pool{
	New: func() interface{} {
		return bufio.NewWriterSize(nil, 4096)
	},
}

// HTTPRequestHandler handles HTTP requests on TCP connections.
type HTTPRequestHandler struct {
	conn         net.Conn
	reader       *bufio.Reader
	httpHandler  http.Handler
	httpListener *connQueueListener
	ctx          interface{ Done() <-chan struct{} }
	logger       *zap.Logger
	mu           *sync.RWMutex
	handedOff    *bool
}

// NewHTTPRequestHandler creates a new HTTP request handler.
func NewHTTPRequestHandler(
	conn net.Conn,
	reader *bufio.Reader,
	httpHandler http.Handler,
	httpListener *connQueueListener,
	ctx interface{ Done() <-chan struct{} },
	logger *zap.Logger,
	mu *sync.RWMutex,
	handedOff *bool,
) *HTTPRequestHandler {
	return &HTTPRequestHandler{
		conn:         conn,
		reader:       reader,
		httpHandler:  httpHandler,
		httpListener: httpListener,
		ctx:          ctx,
		logger:       logger,
		mu:           mu,
		handedOff:    handedOff,
	}
}

// Handle processes the HTTP request.
func (h *HTTPRequestHandler) Handle() error {
	if h.httpListener == nil {
		return h.handleLegacy()
	}

	if err := h.conn.SetReadDeadline(time.Time{}); err != nil {
		h.logger.Warn("Failed to clear read deadline", zap.Error(err))
	}

	wrappedConn := &bufferedConn{
		Conn:   h.conn,
		reader: h.reader,
	}

	if !h.httpListener.Enqueue(wrappedConn) {
		h.logger.Warn("HTTP listener queue full, rejecting connection")
		response := "HTTP/1.1 503 Service Unavailable\r\n" +
			"Content-Type: text/plain\r\n" +
			"Content-Length: 32\r\n" +
			"Connection: close\r\n" +
			"\r\n" +
			"Server busy, please retry later\r\n"
		_, _ = h.conn.Write([]byte(response))
		return fmt.Errorf("http listener queue full")
	}

	h.mu.Lock()
	*h.handedOff = true
	h.mu.Unlock()

	return nil
}

// handleLegacy processes HTTP requests using the legacy handler.
func (h *HTTPRequestHandler) handleLegacy() error {
	if h.httpHandler == nil {
		h.logger.Warn("HTTP request received but no HTTP handler configured")
		response := "HTTP/1.1 503 Service Unavailable\r\n" +
			"Content-Type: text/plain\r\n" +
			"Content-Length: 47\r\n" +
			"\r\n" +
			"HTTP handler not configured for this TCP port\r\n"
		_, _ = h.conn.Write([]byte(response))
		return fmt.Errorf("HTTP handler not configured")
	}

	if err := h.conn.SetReadDeadline(time.Time{}); err != nil {
		h.logger.Warn("Failed to clear read deadline", zap.Error(err))
	}

	for {
		if err := h.conn.SetReadDeadline(time.Now().Add(60 * time.Second)); err != nil {
			h.logger.Warn("Failed to set read deadline", zap.Error(err))
		}

		req, err := http.ReadRequest(h.reader)
		if err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				h.logger.Debug("Client closed HTTP connection")
				return nil
			}
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				h.logger.Debug("HTTP keep-alive timeout")
				return nil
			}
			errStr := err.Error()
			if errors.Is(err, net.ErrClosed) || strings.Contains(errStr, "use of closed network connection") {
				h.logger.Debug("HTTP connection closed during read", zap.Error(err))
				return nil
			}
			if strings.Contains(errStr, "connection reset by peer") ||
				strings.Contains(errStr, "broken pipe") ||
				strings.Contains(errStr, "connection refused") {
				h.logger.Debug("Client disconnected abruptly", zap.Error(err))
				return nil
			}
			if strings.Contains(errStr, "malformed HTTP") {
				h.logger.Warn("Received malformed HTTP request",
					zap.Error(err),
					zap.String("error_snippet", errStr[:min(len(errStr), 100)]),
				)
				return nil
			}
			h.logger.Error("Failed to parse HTTP request", zap.Error(err))
			return fmt.Errorf("failed to parse HTTP request: %w", err)
		}

		if h.ctx != nil {
			if ctx, ok := h.ctx.(interface {
				Done() <-chan struct{}
				Deadline() (deadline time.Time, ok bool)
				Err() error
				Value(key interface{}) interface{}
			}); ok {
				req = req.WithContext(ctx)
			}
		}

		h.logger.Info("Processing HTTP request on TCP port",
			zap.String("method", req.Method),
			zap.String("path", utils.URLPathForLog(req.URL)),
			zap.Strings("query_keys", utils.QueryKeysForLog(req.URL)),
			zap.String("host", req.Host),
		)

		// Get writer from pool to reduce GC pressure
		pooledWriter := httpRespWriterPool.Get().(*bufio.Writer)
		pooledWriter.Reset(h.conn)

		respWriter := &httpResponseWriter{
			conn:   h.conn,
			writer: pooledWriter,
			header: make(http.Header),
		}

		h.httpHandler.ServeHTTP(respWriter, req)

		if err := respWriter.writer.Flush(); err != nil {
			h.logger.Debug("Failed to flush HTTP response", zap.Error(err))
		}

		// Return writer to pool
		pooledWriter.Reset(nil) // Clear reference to connection
		httpRespWriterPool.Put(pooledWriter)

		h.logger.Debug("HTTP request processing completed",
			zap.String("method", req.Method),
			zap.String("path", utils.URLPathForLog(req.URL)),
			zap.Strings("query_keys", utils.QueryKeysForLog(req.URL)),
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
			h.logger.Debug("Closing connection as requested by client or server")
			return nil
		}
	}
}

// httpResponseWriter implements http.ResponseWriter for raw TCP connections.
type httpResponseWriter struct {
	conn          net.Conn
	writer        *bufio.Writer
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

	_, _ = w.writer.WriteString("HTTP/1.1 ")
	_, _ = w.writer.WriteString(fmt.Sprintf("%d", statusCode))
	_ = w.writer.WriteByte(' ')
	_, _ = w.writer.WriteString(statusText)
	_, _ = w.writer.WriteString("\r\n")

	for key, values := range w.header {
		for _, value := range values {
			if strings.ContainsAny(key, "\r\n") || strings.ContainsAny(value, "\r\n") {
				continue
			}
			_, _ = w.writer.WriteString(key)
			_, _ = w.writer.WriteString(": ")
			_, _ = w.writer.WriteString(value)
			_, _ = w.writer.WriteString("\r\n")
		}
	}

	_, _ = w.writer.WriteString("\r\n")
}

func (w *httpResponseWriter) Write(data []byte) (int, error) {
	if !w.headerWritten {
		w.WriteHeader(http.StatusOK)
	}
	return w.writer.Write(data)
}

// Flush implements http.Flusher to prevent panics from handlers that
// type-assert the ResponseWriter.
func (w *httpResponseWriter) Flush() {
	if !w.headerWritten {
		w.WriteHeader(http.StatusOK)
	}
	_ = w.writer.Flush()
}

// Ensure httpResponseWriter implements http.Flusher at compile time.
var _ http.Flusher = (*httpResponseWriter)(nil)
