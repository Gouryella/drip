package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"drip/internal/shared/utils"
)

const reservationColumns = `id, account_id, client_id, tunnel_type, subdomain,
	tcp_port, bandwidth, enabled, created_at, updated_at`

// NormalizeTunnelType maps a protocol tunnel type onto the family a reservation
// is stored under. A reserved subdomain is a name, and the same name serves
// both plain and TLS traffic, so http and https share one family.
func NormalizeTunnelType(tunnelType string) string {
	switch strings.ToLower(strings.TrimSpace(tunnelType)) {
	case TunnelTypeHTTP, TunnelTypeHTTPS:
		return TunnelTypeHTTP
	case TunnelTypeTCP:
		return TunnelTypeTCP
	default:
		return strings.ToLower(strings.TrimSpace(tunnelType))
	}
}

// CreateReservation pins a subdomain or a TCP port to an account.
//
// Exactly one of Subdomain and TCPPort must be set, matching the tunnel type:
// http and https reservations name a subdomain, tcp reservations pin a port.
func (s *Store) CreateReservation(ctx context.Context, r *Reservation) error {
	if r == nil {
		return fmt.Errorf("reservation is required")
	}
	if r.AccountID == "" {
		return fmt.Errorf("account ID is required")
	}

	r.TunnelType = NormalizeTunnelType(r.TunnelType)
	r.Subdomain = strings.ToLower(strings.TrimSpace(r.Subdomain))

	switch r.TunnelType {
	case TunnelTypeHTTP:
		if r.Subdomain == "" {
			return fmt.Errorf("a subdomain is required for http reservations")
		}
		if r.TCPPort != 0 {
			return fmt.Errorf("http reservations cannot pin a TCP port")
		}
		if !utils.ValidateSubdomain(r.Subdomain) {
			return fmt.Errorf("invalid subdomain %q", r.Subdomain)
		}
		if utils.IsReserved(r.Subdomain) {
			return fmt.Errorf("subdomain %q is reserved by the server", r.Subdomain)
		}
	case TunnelTypeTCP:
		if r.TCPPort < 1 || r.TCPPort > 65535 {
			return fmt.Errorf("tcp reservations need a port between 1 and 65535")
		}
		if r.Subdomain != "" {
			return fmt.Errorf("tcp reservations cannot name a subdomain")
		}
	default:
		return fmt.Errorf("unknown tunnel type %q: want http, https or tcp", r.TunnelType)
	}

	if r.ID == "" {
		r.ID = utils.GenerateID()
	}
	now := time.Now()
	r.CreatedAt = now
	r.UpdatedAt = now

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO tunnel_reservations (id, account_id, client_id, tunnel_type,
			subdomain, tcp_port, bandwidth, enabled, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.AccountID, nullString(r.ClientID), r.TunnelType,
		r.Subdomain, r.TCPPort, r.Bandwidth, boolToInt(r.Enabled),
		now.Unix(), now.Unix())
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("reservation for %s: %w", reservationTarget(r), ErrConflict)
		}
		return fmt.Errorf("failed to create reservation: %w", err)
	}
	return nil
}

// GetReservation looks up a reservation by ID.
func (s *Store) GetReservation(ctx context.Context, id string) (*Reservation, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+reservationColumns+` FROM tunnel_reservations WHERE id = ?`, id)
	return scanReservation(row)
}

// GetReservationBySubdomain looks up who owns a subdomain, if anyone.
func (s *Store) GetReservationBySubdomain(ctx context.Context, subdomain string) (*Reservation, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+reservationColumns+` FROM tunnel_reservations WHERE subdomain = ?`,
		strings.ToLower(strings.TrimSpace(subdomain)))
	return scanReservation(row)
}

// GetReservationByTCPPort looks up who owns a TCP port, if anyone.
func (s *Store) GetReservationByTCPPort(ctx context.Context, port int) (*Reservation, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+reservationColumns+` FROM tunnel_reservations WHERE tcp_port = ?`, port)
	return scanReservation(row)
}

// ListReservationsForClient returns the enabled reservations bound to a specific
// client for a tunnel type, oldest first.
//
// Ordering is by creation time so that a client holding several reservations
// binds them in a stable, predictable order across restarts.
func (s *Store) ListReservationsForClient(ctx context.Context, clientID, tunnelType string) ([]*Reservation, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+reservationColumns+` FROM tunnel_reservations
		 WHERE client_id = ? AND tunnel_type = ? AND enabled = 1
		 ORDER BY created_at, id`,
		clientID, NormalizeTunnelType(tunnelType))
	if err != nil {
		return nil, fmt.Errorf("failed to list client reservations: %w", err)
	}
	return collectReservations(rows)
}

// ListReservations returns reservations, optionally filtered to one account.
func (s *Store) ListReservations(ctx context.Context, accountID string) ([]*Reservation, error) {
	query := `SELECT ` + reservationColumns + ` FROM tunnel_reservations`
	args := []interface{}{}
	if accountID != "" {
		query += ` WHERE account_id = ?`
		args = append(args, accountID)
	}
	query += ` ORDER BY tunnel_type, subdomain, tcp_port`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list reservations: %w", err)
	}
	return collectReservations(rows)
}

// UpdateReservation persists the mutable fields of a reservation. The pinned
// target and tunnel type are immutable; delete and recreate to change them.
func (s *Store) UpdateReservation(ctx context.Context, r *Reservation) error {
	if r == nil || r.ID == "" {
		return fmt.Errorf("reservation ID is required")
	}

	res, err := s.db.ExecContext(ctx,
		`UPDATE tunnel_reservations SET client_id = ?, bandwidth = ?, enabled = ?, updated_at = ?
		 WHERE id = ?`,
		nullString(r.ClientID), r.Bandwidth, boolToInt(r.Enabled), time.Now().Unix(), r.ID)
	if err != nil {
		return fmt.Errorf("failed to update reservation: %w", err)
	}
	return checkAffected(res, "reservation")
}

// DeleteReservation releases a pinned subdomain or port.
func (s *Store) DeleteReservation(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM tunnel_reservations WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("failed to delete reservation: %w", err)
	}
	return checkAffected(res, "reservation")
}

// reservationTarget describes what a reservation pins, for error messages.
func reservationTarget(r *Reservation) string {
	if r.Subdomain != "" {
		return "subdomain " + r.Subdomain
	}
	return fmt.Sprintf("tcp port %d", r.TCPPort)
}

// Target returns a human-readable description of what this reservation pins.
func (r *Reservation) Target() string {
	return reservationTarget(r)
}

func collectReservations(rows *sql.Rows) ([]*Reservation, error) {
	defer func() { _ = rows.Close() }()

	var out []*Reservation
	for rows.Next() {
		r, err := scanReservation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to read reservations: %w", err)
	}
	return out, nil
}

func scanReservation(row rowScanner) (*Reservation, error) {
	var (
		r         Reservation
		clientID  sql.NullString
		enabled   int
		createdAt int64
		updatedAt int64
	)
	err := row.Scan(&r.ID, &r.AccountID, &clientID, &r.TunnelType, &r.Subdomain,
		&r.TCPPort, &r.Bandwidth, &enabled, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to scan reservation: %w", err)
	}
	r.ClientID = strPtr(clientID)
	r.Enabled = enabled != 0
	r.CreatedAt = time.Unix(createdAt, 0)
	r.UpdatedAt = time.Unix(updatedAt, 0)
	return &r, nil
}
