package tcp

import (
	"strings"
	"time"

	"drip/internal/shared/protocol"
	"drip/internal/shared/stats"

	"go.uber.org/zap"
)

// TransportType defines the transport protocol for tunnel connections
type TransportType string

const (
	// TransportAuto automatically selects transport based on server address
	TransportAuto TransportType = "auto"
	// TransportTCP uses direct TLS 1.3 connection
	TransportTCP TransportType = "tcp"
	// TransportWebSocket uses WebSocket over TLS (CDN-friendly)
	TransportWebSocket TransportType = "wss"
)

type LatencyCallback func(latency time.Duration)

type ConnectorConfig struct {
	ServerAddr string
	Token      string
	TunnelType protocol.TunnelType
	LocalHost  string
	LocalPort  int
	Subdomain  string
	Insecure   bool

	PoolSize int
	PoolMin  int
	PoolMax  int

	AllowIPs []string
	DenyIPs  []string

	// Proxy authentication
	AuthPass string

	// Transport protocol selection
	Transport TransportType

	// Bandwidth limiting (bytes/sec), 0 = unlimited
	Bandwidth int64
}

type TunnelClient interface {
	Connect() error
	Close() error
	Wait()
	GetURL() string
	GetSubdomain() string
	SetLatencyCallback(cb LatencyCallback)
	GetLatency() time.Duration
	GetStats() *stats.TrafficStats
	IsClosed() bool
}

func NewTunnelClient(cfg *ConnectorConfig, logger *zap.Logger) TunnelClient {
	return NewPoolClient(cfg, logger)
}

func isExpectedCloseError(err error) bool {
	s := err.Error()
	return strings.Contains(s, "EOF") ||
		strings.Contains(s, "use of closed") ||
		strings.Contains(s, "connection reset")
}
