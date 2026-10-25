package dmux

import (
	"errors"
	"time"
)

var (
	ErrSessionClosed = errors.New("dmux: session closed")
	ErrStreamClosed  = errors.New("dmux: stream closed")
	ErrStreamReset   = errors.New("dmux: stream reset")
	ErrTimeout       = errors.New("dmux: timeout")
	ErrProtocol      = errors.New("dmux: protocol error")
	ErrGoAway        = errors.New("dmux: session is going away")
)

// Config controls session behavior. Zero values are replaced by defaults.
type Config struct {
	// AcceptBacklog bounds streams waiting in Accept.
	AcceptBacklog int
	// MaxStreamWindowSize is the per-stream receive window. Both peers must match.
	MaxStreamWindowSize uint32
	// ConnectionWriteTimeout bounds a single write to the transport.
	ConnectionWriteTimeout time.Duration
	// EnableKeepAlive runs periodic PING frames.
	EnableKeepAlive bool
	// KeepAliveInterval is the PING cadence.
	KeepAliveInterval time.Duration
	// KeepAliveTimeout is how long a PING may stay unanswered before the
	// session is closed.
	KeepAliveTimeout time.Duration
}

// DefaultConfig returns conservative defaults suitable for tunnel traffic.
func DefaultConfig() *Config {
	return &Config{
		AcceptBacklog:          acceptBacklog,
		MaxStreamWindowSize:    initialWindow,
		ConnectionWriteTimeout: 30 * time.Second,
		EnableKeepAlive:        true,
		KeepAliveInterval:      15 * time.Second,
		KeepAliveTimeout:       10 * time.Second,
	}
}

func (c *Config) normalize() {
	if c == nil {
		return
	}
	if c.AcceptBacklog <= 0 {
		c.AcceptBacklog = acceptBacklog
	}
	if c.MaxStreamWindowSize == 0 {
		c.MaxStreamWindowSize = initialWindow
	}
	if c.MaxStreamWindowSize > maxWindow {
		c.MaxStreamWindowSize = maxWindow
	}
	if c.ConnectionWriteTimeout <= 0 {
		c.ConnectionWriteTimeout = 30 * time.Second
	}
	if c.KeepAliveInterval <= 0 {
		c.KeepAliveInterval = 15 * time.Second
	}
	if c.KeepAliveTimeout <= 0 {
		c.KeepAliveTimeout = 10 * time.Second
	}
}
