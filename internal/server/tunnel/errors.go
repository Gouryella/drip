package tunnel

import "errors"

var (
	// ErrConnectionClosed is returned when trying to use a closed connection
	ErrConnectionClosed = errors.New("connection is closed")

	// ErrSendTimeout is returned when send operation times out
	ErrSendTimeout = errors.New("send operation timed out")

	// ErrTunnelNotFound is returned when a tunnel is not found
	ErrTunnelNotFound = errors.New("tunnel not found")

	// ErrSubdomainTaken is returned when a subdomain is already in use
	ErrSubdomainTaken = errors.New("subdomain is already taken")

	// ErrInvalidSubdomain is returned when a subdomain is invalid
	ErrInvalidSubdomain = errors.New("invalid subdomain format")

	// ErrReservedSubdomain is returned when trying to use a reserved subdomain
	ErrReservedSubdomain = errors.New("subdomain is reserved")

	// ErrTooManyForAccount is returned when an account is at its tunnel limit
	ErrTooManyForAccount = errors.New("maximum tunnels for this account reached")
)
