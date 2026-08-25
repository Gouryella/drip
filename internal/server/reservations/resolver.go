// Package reservations decides which subdomain or TCP port a registering client
// is entitled to, given the reservations recorded in the control plane.
package reservations

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"drip/internal/server/store"

	"go.uber.org/zap"
)

// Resolution failures. Each is reported to the client verbatim, because unlike
// authentication these are actionable configuration problems, not probes.
var (
	// ErrReservedByAnother means the requested name belongs to another account.
	ErrReservedByAnother = errors.New("subdomain is reserved by another account")
	// ErrPortReservedByAnother means the requested port belongs to another account.
	ErrPortReservedByAnother = errors.New("port is reserved by another account")
	// ErrReservationDisabled means the reservation exists but is turned off.
	ErrReservationDisabled = errors.New("reservation is disabled")
	// ErrReservationRequired means the server only accepts reserved tunnels.
	ErrReservationRequired = errors.New("this server only accepts reserved tunnels; ask an administrator to reserve one for this client")
	// ErrReservationInUse means every reservation this client holds is already
	// bound by another live tunnel.
	ErrReservationInUse = errors.New("every reservation for this client is already in use")
	// ErrNotBoundToClient means the reservation is pinned to a different client.
	ErrNotBoundToClient = errors.New("reservation is bound to a different client credential")
)

// Request describes a registration awaiting a name.
type Request struct {
	// AccountID and ClientID identify the authenticated client. Both empty for
	// legacy shared-token and anonymous registrations, which own nothing.
	AccountID string
	ClientID  string
	// TunnelType is the protocol tunnel type: http, https or tcp.
	TunnelType string
	// RequestedSubdomain is what the client asked for, if anything. For TCP
	// tunnels this may encode a port as "tcp-<port>".
	RequestedSubdomain string
	// RequestedTCPPort is a specific port the client asked for, if any.
	RequestedTCPPort int
}

// Resolution is the outcome: what the client may bind.
type Resolution struct {
	// Subdomain is the name to register, or "" to generate a random one.
	Subdomain string
	// TCPPort is the port to allocate, or 0 to allocate any free port.
	TCPPort int
	// ReservationID is set when this registration is bound to a reservation.
	ReservationID string
	// Bandwidth is the reservation's per-tunnel override, if it sets one.
	Bandwidth string
}

// IsReserved reports whether the registration bound a reservation.
func (r *Resolution) IsReserved() bool {
	return r != nil && r.ReservationID != ""
}

// ActiveChecker reports whether a subdomain is currently registered. The
// resolver uses it to skip reservations that a live tunnel already holds.
type ActiveChecker func(subdomain string) bool

// Resolver applies reservation policy to registrations.
type Resolver struct {
	store *store.Store
	// requireReservation rejects any registration that does not bind a
	// reservation, turning the server into a closed fleet.
	requireReservation bool
	logger             *zap.Logger
}

// New builds a Resolver. A nil store disables reservation handling entirely and
// every registration resolves to an ephemeral tunnel.
func New(s *store.Store, requireReservation bool, logger *zap.Logger) *Resolver {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Resolver{store: s, requireReservation: requireReservation, logger: logger}
}

// Enabled reports whether reservations are in play.
func (r *Resolver) Enabled() bool {
	return r != nil && r.store != nil
}

// Resolve decides what the request may bind.
//
// The order is: an explicitly requested name is checked against its reservation
// and granted or refused; a client that asks for nothing is auto-bound to a
// reservation it owns; anything left over becomes an ephemeral tunnel, unless
// the server requires reservations.
func (r *Resolver) Resolve(ctx context.Context, req Request, isActive ActiveChecker) (*Resolution, error) {
	if !r.Enabled() {
		if r != nil && r.requireReservation {
			return nil, ErrReservationRequired
		}
		return &Resolution{Subdomain: req.RequestedSubdomain, TCPPort: req.RequestedTCPPort}, nil
	}

	if isActive == nil {
		isActive = func(string) bool { return false }
	}

	family := store.NormalizeTunnelType(req.TunnelType)

	if family == store.TunnelTypeTCP {
		return r.resolveTCP(ctx, req, isActive)
	}
	return r.resolveHTTP(ctx, req, isActive)
}

func (r *Resolver) resolveHTTP(ctx context.Context, req Request, isActive ActiveChecker) (*Resolution, error) {
	requested := strings.ToLower(strings.TrimSpace(req.RequestedSubdomain))

	if requested != "" {
		reservation, err := r.store.GetReservationBySubdomain(ctx, requested)
		if errors.Is(err, store.ErrNotFound) {
			// Nobody owns this name. Allow it as an ephemeral tunnel unless the
			// server is closed to unreserved traffic.
			if r.requireReservation {
				return nil, ErrReservationRequired
			}
			return &Resolution{Subdomain: requested}, nil
		}
		if err != nil {
			return nil, err
		}

		if err := r.checkOwnership(reservation, req, ErrReservedByAnother); err != nil {
			return nil, err
		}
		return resolutionFor(reservation), nil
	}

	// No name requested: bind a reservation this client owns, if it has one.
	resolution, err := r.autoBind(ctx, req, store.TunnelTypeHTTP, isActive)
	if err != nil || resolution != nil {
		return resolution, err
	}

	if r.requireReservation {
		return nil, ErrReservationRequired
	}
	return &Resolution{}, nil
}

func (r *Resolver) resolveTCP(ctx context.Context, req Request, isActive ActiveChecker) (*Resolution, error) {
	if req.RequestedTCPPort > 0 {
		reservation, err := r.store.GetReservationByTCPPort(ctx, req.RequestedTCPPort)
		if errors.Is(err, store.ErrNotFound) {
			if r.requireReservation {
				return nil, ErrReservationRequired
			}
			return &Resolution{TCPPort: req.RequestedTCPPort, Subdomain: req.RequestedSubdomain}, nil
		}
		if err != nil {
			return nil, err
		}

		if err := r.checkOwnership(reservation, req, ErrPortReservedByAnother); err != nil {
			return nil, err
		}
		return resolutionFor(reservation), nil
	}

	resolution, err := r.autoBind(ctx, req, store.TunnelTypeTCP, isActive)
	if err != nil || resolution != nil {
		return resolution, err
	}

	if r.requireReservation {
		return nil, ErrReservationRequired
	}
	return &Resolution{Subdomain: req.RequestedSubdomain}, nil
}

// autoBind picks the first reservation the client owns that no live tunnel is
// currently holding. It returns (nil, nil) when the client owns none, which the
// caller reads as "fall through to an ephemeral tunnel".
func (r *Resolver) autoBind(ctx context.Context, req Request, family string, isActive ActiveChecker) (*Resolution, error) {
	if req.ClientID == "" {
		return nil, nil
	}

	owned, err := r.store.ListReservationsForClient(ctx, req.ClientID, family)
	if err != nil {
		return nil, err
	}
	if len(owned) == 0 {
		return nil, nil
	}

	for _, reservation := range owned {
		if !isActive(reservationSubdomain(reservation)) {
			return resolutionFor(reservation), nil
		}
	}

	// The client holds reservations but every one is live. Say so rather than
	// silently handing out a random subdomain, which would look like the
	// reservation had been lost.
	return nil, ErrReservationInUse
}

// checkOwnership verifies the requester may bind this reservation.
func (r *Resolver) checkOwnership(reservation *store.Reservation, req Request, mismatch error) error {
	if reservation.AccountID != req.AccountID || req.AccountID == "" {
		return fmt.Errorf("%s: %w", reservation.Target(), mismatch)
	}
	if !reservation.Enabled {
		return fmt.Errorf("%s: %w", reservation.Target(), ErrReservationDisabled)
	}
	if reservation.ClientID != nil && *reservation.ClientID != req.ClientID {
		return fmt.Errorf("%s: %w", reservation.Target(), ErrNotBoundToClient)
	}
	return nil
}

// reservationSubdomain is the manager key a reservation occupies when live.
func reservationSubdomain(reservation *store.Reservation) string {
	if reservation.Subdomain != "" {
		return reservation.Subdomain
	}
	return fmt.Sprintf("tcp-%d", reservation.TCPPort)
}

func resolutionFor(reservation *store.Reservation) *Resolution {
	return &Resolution{
		Subdomain:     reservationSubdomain(reservation),
		TCPPort:       reservation.TCPPort,
		ReservationID: reservation.ID,
		Bandwidth:     reservation.Bandwidth,
	}
}
