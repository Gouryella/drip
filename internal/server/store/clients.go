package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const clientColumns = `id, account_id, name, secret_hash, enabled, bandwidth,
	last_seen_at, last_seen_ip, created_at, updated_at`

// CreateClient inserts a client credential. The caller generates the ID and the
// secret hash (see internal/server/auth) so the plaintext secret never reaches
// the store.
func (s *Store) CreateClient(ctx context.Context, c *Client) error {
	if c == nil {
		return fmt.Errorf("client is required")
	}
	if c.ID == "" || c.SecretHash == "" {
		return fmt.Errorf("client ID and secret hash are required")
	}
	c.Name = strings.TrimSpace(c.Name)
	if c.Name == "" {
		return fmt.Errorf("client name is required")
	}
	if c.AccountID == "" {
		return fmt.Errorf("account ID is required")
	}

	now := time.Now()
	c.CreatedAt = now
	c.UpdatedAt = now

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO clients (id, account_id, name, secret_hash, enabled, bandwidth,
			last_seen_ip, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.AccountID, c.Name, c.SecretHash, boolToInt(c.Enabled), c.Bandwidth,
		c.LastSeenIP, now.Unix(), now.Unix())
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("client %q: %w", c.Name, ErrConflict)
		}
		return fmt.Errorf("failed to create client: %w", err)
	}
	return nil
}

// GetClient looks up a client by its credential ID. This is the registration
// hot path lookup.
func (s *Store) GetClient(ctx context.Context, id string) (*Client, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+clientColumns+` FROM clients WHERE id = ?`, id)
	return scanClient(row)
}

// ListClients returns clients, optionally filtered to one account.
func (s *Store) ListClients(ctx context.Context, accountID string) ([]*Client, error) {
	query := `SELECT ` + clientColumns + ` FROM clients`
	args := []interface{}{}
	if accountID != "" {
		query += ` WHERE account_id = ?`
		args = append(args, accountID)
	}
	query += ` ORDER BY name`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list clients: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*Client
	for rows.Next() {
		c, err := scanClient(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to read clients: %w", err)
	}
	return out, nil
}

// UpdateClient persists the mutable fields of a client. The secret hash is not
// touched here; use RotateClientSecret.
func (s *Store) UpdateClient(ctx context.Context, c *Client) error {
	if c == nil || c.ID == "" {
		return fmt.Errorf("client ID is required")
	}

	res, err := s.db.ExecContext(ctx,
		`UPDATE clients SET name = ?, enabled = ?, bandwidth = ?, updated_at = ?
		 WHERE id = ?`,
		c.Name, boolToInt(c.Enabled), c.Bandwidth, time.Now().Unix(), c.ID)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("client %q: %w", c.Name, ErrConflict)
		}
		return fmt.Errorf("failed to update client: %w", err)
	}
	return checkAffected(res, "client")
}

// RotateClientSecret replaces the stored secret hash, invalidating the old
// credential immediately.
func (s *Store) RotateClientSecret(ctx context.Context, id, secretHash string) error {
	if secretHash == "" {
		return fmt.Errorf("secret hash is required")
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE clients SET secret_hash = ?, updated_at = ? WHERE id = ?`,
		secretHash, time.Now().Unix(), id)
	if err != nil {
		return fmt.Errorf("failed to rotate client secret: %w", err)
	}
	return checkAffected(res, "client")
}

// DeleteClient removes a client credential.
func (s *Store) DeleteClient(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM clients WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("failed to delete client: %w", err)
	}
	return checkAffected(res, "client")
}

// TouchClient records a successful registration. Failures are not fatal to the
// tunnel, so callers may log and continue.
func (s *Store) TouchClient(ctx context.Context, id, remoteIP string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE clients SET last_seen_at = ?, last_seen_ip = ? WHERE id = ?`,
		time.Now().Unix(), remoteIP, id)
	if err != nil {
		return fmt.Errorf("failed to update client last seen: %w", err)
	}
	return nil
}

func scanClient(row rowScanner) (*Client, error) {
	var (
		c          Client
		enabled    int
		lastSeenAt sql.NullInt64
		createdAt  int64
		updatedAt  int64
	)
	err := row.Scan(&c.ID, &c.AccountID, &c.Name, &c.SecretHash, &enabled, &c.Bandwidth,
		&lastSeenAt, &c.LastSeenIP, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to scan client: %w", err)
	}
	c.Enabled = enabled != 0
	c.LastSeenAt = unixPtr(lastSeenAt)
	c.CreatedAt = time.Unix(createdAt, 0)
	c.UpdatedAt = time.Unix(updatedAt, 0)
	return &c, nil
}
