package constants

import "time"

const (
	// DefaultServerPort is the default port for the tunnel server
	DefaultServerPort = 8080

	// DefaultWSPort is the default WebSocket port
	DefaultWSPort = 8080

	// ==================== Yamux Configuration ====================
	// These settings are tuned for high-throughput tunnel scenarios

	// YamuxAcceptBacklog controls how many incoming streams can be queued
	// before yamux starts blocking stream opens under load.
	// Increased from default 256 to handle burst traffic.
	YamuxAcceptBacklog = 8192

	// YamuxMaxStreamWindowSize is the maximum window size for a stream.
	// Larger windows allow more data in-flight, improving throughput on high-latency links.
	// Default is 256KB, increased to 512KB for better throughput.
	YamuxMaxStreamWindowSize = 512 * 1024

	// YamuxStreamOpenTimeout is how long to wait for a stream open to complete.
	YamuxStreamOpenTimeout = 10 * time.Second

	// YamuxStreamCloseTimeout is how long to wait for a stream close to complete.
	YamuxStreamCloseTimeout = 5 * time.Minute

	// YamuxKeepAliveInterval is how often to send keep-alive pings.
	// Set higher than HeartbeatInterval to avoid redundant pings.
	YamuxKeepAliveInterval = 15 * time.Second

	// YamuxConnectionWriteTimeout is the timeout for writing to the underlying connection.
	YamuxConnectionWriteTimeout = 10 * time.Second

	// ==================== Heartbeat Configuration ====================

	// HeartbeatInterval is how often clients send heartbeat messages
	HeartbeatInterval = 2 * time.Second

	// HeartbeatTimeout is how long the server waits before considering a connection dead
	HeartbeatTimeout = 6 * time.Second

	// ==================== Request/Response Timeouts ====================

	// RequestTimeout is the maximum time to wait for a response from the client
	RequestTimeout = 30 * time.Second

	// ==================== Reconnection Configuration ====================

	// ReconnectBaseDelay is the initial delay for reconnection attempts
	ReconnectBaseDelay = 1 * time.Second

	// ReconnectMaxDelay is the maximum delay between reconnection attempts
	ReconnectMaxDelay = 60 * time.Second

	// MaxReconnectAttempts is the maximum number of reconnection attempts (0 = infinite)
	MaxReconnectAttempts = 0

	// ==================== TCP Port Allocation ====================

	// DefaultTCPPortMin/Max define the default allocation range for TCP tunnels
	DefaultTCPPortMin = 20000
	DefaultTCPPortMax = 40000

	// DefaultDomain is the default domain for tunnels
	DefaultDomain = "tunnel.localhost"
)

// Error codes
const (
	ErrCodeTunnelNotFound   = "TUNNEL_NOT_FOUND"
	ErrCodeTimeout          = "TIMEOUT"
	ErrCodeConnectionFailed = "CONNECTION_FAILED"
	ErrCodeInvalidRequest   = "INVALID_REQUEST"
	ErrCodeAuthFailed       = "AUTH_FAILED"
	ErrCodeRateLimited      = "RATE_LIMITED"
)
