package mux

import (
	"io"
	"net"

	"drip/internal/shared/constants"
	"drip/internal/shared/mux/dmux"
)

// Session, Stream and Config alias the in-house multiplexer so call sites do
// not depend on a third-party implementation.
type (
	Session = dmux.Session
	Stream  = dmux.Stream
	Config  = dmux.Config
)

// ErrGoAway is returned by Open once either peer has sent GOAWAY.
var ErrGoAway = dmux.ErrGoAway

// NewOptimizedConfig returns a multiplexer config tuned for tunnel traffic.
func NewOptimizedConfig() *Config {
	cfg := dmux.DefaultConfig()
	cfg.AcceptBacklog = constants.MuxAcceptBacklog
	cfg.MaxStreamWindowSize = constants.MuxMaxStreamWindowSize
	cfg.ConnectionWriteTimeout = constants.MuxConnectionWriteTimeout
	cfg.EnableKeepAlive = true
	cfg.KeepAliveInterval = constants.MuxKeepAliveInterval
	cfg.LogOutput = io.Discard
	return cfg
}

// NewServerConfig returns a multiplexer config for server-side use.
func NewServerConfig() *Config { return NewOptimizedConfig() }

// NewClientConfig returns a multiplexer config for client-side use.
func NewClientConfig() *Config {
	cfg := NewOptimizedConfig()
	cfg.EnableKeepAlive = false
	return cfg
}

// Client creates a session that opens odd-numbered streams.
func Client(conn net.Conn, cfg *Config) (*Session, error) { return dmux.Client(conn, cfg) }

// Server creates a session that opens even-numbered streams.
func Server(conn net.Conn, cfg *Config) (*Session, error) { return dmux.Server(conn, cfg) }
