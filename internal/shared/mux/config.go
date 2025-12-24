package mux

import (
	"io"

	"github.com/hashicorp/yamux"

	"drip/internal/shared/constants"
)

// NewOptimizedConfig returns a multiplexer config optimized for tunnel scenarios.
func NewOptimizedConfig() *yamux.Config {
	cfg := yamux.DefaultConfig()
	cfg.AcceptBacklog = constants.YamuxAcceptBacklog
	cfg.MaxStreamWindowSize = constants.YamuxMaxStreamWindowSize
	cfg.StreamOpenTimeout = constants.YamuxStreamOpenTimeout
	cfg.StreamCloseTimeout = constants.YamuxStreamCloseTimeout
	cfg.ConnectionWriteTimeout = constants.YamuxConnectionWriteTimeout
	cfg.EnableKeepAlive = true
	cfg.KeepAliveInterval = constants.YamuxKeepAliveInterval
	cfg.LogOutput = io.Discard
	return cfg
}

// NewServerConfig returns a multiplexer config for server-side use.
func NewServerConfig() *yamux.Config {
	return NewOptimizedConfig()
}

// NewClientConfig returns a multiplexer config for client-side use.
func NewClientConfig() *yamux.Config {
	cfg := NewOptimizedConfig()
	cfg.EnableKeepAlive = false
	return cfg
}
