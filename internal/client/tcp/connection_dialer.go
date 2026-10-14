package tcp

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	json "github.com/goccy/go-json"
	"github.com/gorilla/websocket"
	"go.uber.org/zap"

	"drip/internal/shared/wsutil"
)

// serverCapabilities holds the discovered server capabilities
type serverCapabilities struct {
	Transports []string `json:"transports"`
	Preferred  string   `json:"preferred"`
}

type connectionDialer interface {
	DialContext(context.Context) (net.Conn, error)
}

// ConnectionDialer handles establishing connections to the server.
type ConnectionDialer struct {
	serverAddr   string
	tlsConfig    *tls.Config
	token        string
	transport    TransportType
	logger       *zap.Logger
	discoverOnce sync.Once
	capabilities *serverCapabilities
}

// NewConnectionDialer creates a new connection dialer.
func NewConnectionDialer(
	serverAddr string,
	tlsConfig *tls.Config,
	token string,
	transport TransportType,
	logger *zap.Logger,
) *ConnectionDialer {
	if strings.HasPrefix(serverAddr, "wss://") && (transport == TransportAuto || transport == "") {
		transport = TransportWebSocket
	}
	return &ConnectionDialer{
		serverAddr: normalizeServerAddress(serverAddr),
		tlsConfig:  tlsConfig,
		token:      token,
		transport:  transport,
		logger:     logger,
	}
}

// Dial establishes a connection using the appropriate transport.
func (d *ConnectionDialer) Dial() (net.Conn, error) {
	return d.DialContext(context.Background())
}

func (d *ConnectionDialer) DialContext(ctx context.Context) (net.Conn, error) {
	switch d.transport {
	case TransportWebSocket:
		return d.dialWebSocket(ctx)
	case TransportTCP:
		// User explicitly requested TCP, verify server supports it
		caps := d.discoverServerCapabilities(ctx)
		if caps != nil && len(caps.Transports) > 0 {
			tcpSupported := false
			for _, t := range caps.Transports {
				if t == "tcp" {
					tcpSupported = true
					break
				}
			}
			if !tcpSupported {
				return nil, fmt.Errorf("server only supports %v transport(s), but --transport tcp was specified. Use --transport wss instead", caps.Transports)
			}
		}
		return d.dialTLS(ctx)
	default: // TransportAuto
		// Check if server address indicates WebSocket
		if strings.HasPrefix(d.serverAddr, "wss://") {
			return d.dialWebSocket(ctx)
		}
		// Query server for preferred transport
		caps := d.discoverServerCapabilities(ctx)
		if caps != nil && caps.Preferred == "wss" {
			return d.dialWebSocket(ctx)
		}
		// Default to TCP
		return d.dialTLS(ctx)
	}
}

// dialTLS establishes a TLS connection to the server.
func (d *ConnectionDialer) dialTLS(ctx context.Context) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	rawConn, err := (&tls.Dialer{NetDialer: dialer, Config: d.tlsConfig}).DialContext(ctx, "tcp", d.serverAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to connect: %w", err)
	}
	conn := rawConn.(*tls.Conn)

	state := conn.ConnectionState()
	if state.Version != tls.VersionTLS13 {
		_ = conn.Close()
		return nil, fmt.Errorf("server not using TLS 1.3 (version: 0x%04x)", state.Version)
	}

	if tcpConn, ok := conn.NetConn().(*net.TCPConn); ok {
		_ = tcpConn.SetNoDelay(true)
		_ = tcpConn.SetKeepAlive(true)
		_ = tcpConn.SetKeepAlivePeriod(30 * time.Second)
		_ = tcpConn.SetReadBuffer(256 * 1024)
		_ = tcpConn.SetWriteBuffer(256 * 1024)
	}

	return conn, nil
}

// dialWebSocket establishes a WebSocket connection to the server over TLS.
func (d *ConnectionDialer) dialWebSocket(ctx context.Context) (net.Conn, error) {
	wsURL := (&url.URL{Scheme: "wss", Host: d.serverAddr, Path: "/_drip/ws"}).String()

	d.logger.Debug("Connecting via WebSocket over TLS",
		zap.String("url", wsURL),
	)

	dialer := websocket.Dialer{
		TLSClientConfig:  d.tlsConfig,
		HandshakeTimeout: 10 * time.Second,
		ReadBufferSize:   256 * 1024,
		WriteBufferSize:  256 * 1024,
	}

	// Add authorization header if token is set
	header := http.Header{}
	if d.token != "" {
		header.Set("Authorization", "Bearer "+d.token)
	}

	ws, resp, err := dialer.DialContext(ctx, wsURL, header)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		if resp != nil {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("WebSocket dial failed (status %d): %w", resp.StatusCode, err)
		}
		return nil, fmt.Errorf("WebSocket dial failed: %w", err)
	}
	if resp != nil {
		_ = resp.Body.Close()
	}

	d.logger.Info("Connected via WebSocket over TLS",
		zap.String("url", wsURL),
	)

	// Wrap WebSocket connection to implement net.Conn with ping loop for CDN keep-alive
	return wsutil.NewConnWithPing(ws, 30*time.Second), nil
}

// discoverServerCapabilities queries the server for its capabilities.
func (d *ConnectionDialer) discoverServerCapabilities(ctx context.Context) *serverCapabilities {
	d.discoverOnce.Do(func() { d.capabilities = d.fetchServerCapabilities(ctx) })
	return d.capabilities
}

func (d *ConnectionDialer) fetchServerCapabilities(ctx context.Context) *serverCapabilities {
	discoverURL := (&url.URL{Scheme: "https", Host: d.serverAddr, Path: "/_drip/discover"}).String()

	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: d.tlsConfig,
		},
	}
	defer client.CloseIdleConnections()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, discoverURL, nil)
	if err != nil {
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		d.logger.Debug("Failed to discover server capabilities",
			zap.Error(err),
		)
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil
	}

	var caps serverCapabilities
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&caps); err != nil {
		return nil
	}

	d.logger.Debug("Discovered server capabilities",
		zap.Strings("transports", caps.Transports),
		zap.String("preferred", caps.Preferred),
	)

	return &caps
}

func normalizeServerAddress(address string) string {
	if strings.HasPrefix(address, "wss://") {
		if u, err := url.Parse(address); err == nil && u.Host != "" {
			port := u.Port()
			if port == "" {
				port = "443"
			}
			return net.JoinHostPort(u.Hostname(), port)
		}
	}
	if _, _, err := net.SplitHostPort(address); err == nil {
		return address
	}
	return net.JoinHostPort(strings.Trim(address, "[]"), "443")
}
