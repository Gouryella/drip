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

const accountColumns = `id, name, enabled, max_tunnels, created_at, updated_at`

// CreateAccount inserts a new account and returns it.
func (s *Store) CreateAccount(ctx context.Context, name string, maxTunnels int) (*Account, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("account name is required")
	}
	if maxTunnels < 0 {
		return nil, fmt.Errorf("max_tunnels must be >= 0")
	}

	now := time.Now()
	acct := &Account{
		ID:         utils.GenerateID(),
		Name:       name,
		Enabled:    true,
		MaxTunnels: maxTunnels,
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO accounts (id, name, enabled, max_tunnels, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		acct.ID, acct.Name, boolToInt(acct.Enabled), acct.MaxTunnels,
		now.Unix(), now.Unix())
	if err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("account %q: %w", name, ErrConflict)
		}
		return nil, fmt.Errorf("failed to create account: %w", err)
	}

	return acct, nil
}

// GetAccount looks up an account by ID.
func (s *Store) GetAccount(ctx context.Context, id string) (*Account, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+accountColumns+` FROM accounts WHERE id = ?`, id)
	return scanAccount(row)
}

// GetAccountByName looks up an account by its unique name.
func (s *Store) GetAccountByName(ctx context.Context, name string) (*Account, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+accountColumns+` FROM accounts WHERE name = ?`, name)
	return scanAccount(row)
}

// ListAccounts returns every account ordered by name.
func (s *Store) ListAccounts(ctx context.Context) ([]*Account, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+accountColumns+` FROM accounts ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("failed to list accounts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*Account
	for rows.Next() {
		acct, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, acct)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to read accounts: %w", err)
	}
	return out, nil
}

// UpdateAccount persists name, enabled and max_tunnels for an existing account.
func (s *Store) UpdateAccount(ctx context.Context, acct *Account) error {
	if acct == nil || acct.ID == "" {
		return fmt.Errorf("account ID is required")
	}
	if acct.MaxTunnels < 0 {
		return fmt.Errorf("max_tunnels must be >= 0")
	}

	res, err := s.db.ExecContext(ctx,
		`UPDATE accounts SET name = ?, enabled = ?, max_tunnels = ?, updated_at = ?
		 WHERE id = ?`,
		acct.Name, boolToInt(acct.Enabled), acct.MaxTunnels, time.Now().Unix(), acct.ID)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("account %q: %w", acct.Name, ErrConflict)
		}
		return fmt.Errorf("failed to update account: %w", err)
	}
	return checkAffected(res, "account")
}

// DeleteAccount removes an account; clients and reservations cascade.
func (s *Store) DeleteAccount(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM accounts WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("failed to delete account: %w", err)
	}
	return checkAffected(res, "account")
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...interface{}) error
}

func scanAccount(row rowScanner) (*Account, error) {
	var (
		acct      Account
		enabled   int
		createdAt int64
		updatedAt int64
	)
	err := row.Scan(&acct.ID, &acct.Name, &enabled, &acct.MaxTunnels, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to scan account: %w", err)
	}
	acct.Enabled = enabled != 0
	acct.CreatedAt = time.Unix(createdAt, 0)
	acct.UpdatedAt = time.Unix(updatedAt, 0)
	return &acct, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func checkAffected(res sql.Result, what string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to read %s update result: %w", what, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
