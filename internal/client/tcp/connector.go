package tcp

import (
	"strings"
	"time"

	"drip/internal/shared/protocol"
	"drip/internal/shared/stats"

	"go.uber.org/zap"
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
