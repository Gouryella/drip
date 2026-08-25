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

const adminUserColumns = `id, username, password_hash, role, enabled,
	last_login_at, created_at, updated_at`

// ValidRole reports whether role is one the admin panel understands.
func ValidRole(role string) bool {
	switch role {
	case RoleAdmin, RoleViewer:
		return true
	default:
		return false
	}
}

// CreateAdminUser inserts an operator account. The caller hashes the password
// with Argon2id (see internal/server/auth) so plaintext never reaches the store.
func (s *Store) CreateAdminUser(ctx context.Context, u *AdminUser) error {
	if u == nil {
		return fmt.Errorf("admin user is required")
	}
	u.Username = strings.ToLower(strings.TrimSpace(u.Username))
	if u.Username == "" {
		return fmt.Errorf("username is required")
	}
	if u.PasswordHash == "" {
		return fmt.Errorf("password hash is required")
	}
	if u.Role == "" {
		u.Role = RoleAdmin
	}
	if !ValidRole(u.Role) {
		return fmt.Errorf("unknown role %q: want %s or %s", u.Role, RoleAdmin, RoleViewer)
	}

	if u.ID == "" {
		u.ID = utils.GenerateID()
	}
	now := time.Now()
	u.CreatedAt = now
	u.UpdatedAt = now

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO admin_users (id, username, password_hash, role, enabled, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		u.ID, u.Username, u.PasswordHash, u.Role, boolToInt(u.Enabled), now.Unix(), now.Unix())
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("admin user %q: %w", u.Username, ErrConflict)
		}
		return fmt.Errorf("failed to create admin user: %w", err)
	}
	return nil
}

// GetAdminUser looks up an operator by ID.
func (s *Store) GetAdminUser(ctx context.Context, id string) (*AdminUser, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+adminUserColumns+` FROM admin_users WHERE id = ?`, id)
	return scanAdminUser(row)
}

// GetAdminUserByUsername looks up an operator by login name.
func (s *Store) GetAdminUserByUsername(ctx context.Context, username string) (*AdminUser, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+adminUserColumns+` FROM admin_users WHERE username = ?`,
		strings.ToLower(strings.TrimSpace(username)))
	return scanAdminUser(row)
}

// ListAdminUsers returns every operator, ordered by username.
func (s *Store) ListAdminUsers(ctx context.Context) ([]*AdminUser, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+adminUserColumns+` FROM admin_users ORDER BY username`)
	if err != nil {
		return nil, fmt.Errorf("failed to list admin users: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*AdminUser
	for rows.Next() {
		u, err := scanAdminUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to read admin users: %w", err)
	}
	return out, nil
}

// CountAdminUsers reports how many operator accounts exist. The admin server
// uses it to detect a fresh deployment that still needs its first user.
func (s *Store) CountAdminUsers(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM admin_users`).Scan(&n); err != nil {
		return 0, fmt.Errorf("failed to count admin users: %w", err)
	}
	return n, nil
}

// UpdateAdminUser persists role and enabled state.
func (s *Store) UpdateAdminUser(ctx context.Context, u *AdminUser) error {
	if u == nil || u.ID == "" {
		return fmt.Errorf("admin user ID is required")
	}
	if !ValidRole(u.Role) {
		return fmt.Errorf("unknown role %q: want %s or %s", u.Role, RoleAdmin, RoleViewer)
	}

	res, err := s.db.ExecContext(ctx,
		`UPDATE admin_users SET role = ?, enabled = ?, updated_at = ? WHERE id = ?`,
		u.Role, boolToInt(u.Enabled), time.Now().Unix(), u.ID)
	if err != nil {
		return fmt.Errorf("failed to update admin user: %w", err)
	}
	return checkAffected(res, "admin user")
}

// SetAdminPassword replaces an operator's password hash.
func (s *Store) SetAdminPassword(ctx context.Context, id, passwordHash string) error {
	if passwordHash == "" {
		return fmt.Errorf("password hash is required")
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE admin_users SET password_hash = ?, updated_at = ? WHERE id = ?`,
		passwordHash, time.Now().Unix(), id)
	if err != nil {
		return fmt.Errorf("failed to set admin password: %w", err)
	}
	return checkAffected(res, "admin user")
}

// TouchAdminLogin records a successful sign-in.
func (s *Store) TouchAdminLogin(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE admin_users SET last_login_at = ? WHERE id = ?`, time.Now().Unix(), id)
	if err != nil {
		return fmt.Errorf("failed to record admin login: %w", err)
	}
	return nil
}

// DeleteAdminUser removes an operator; their sessions cascade away.
func (s *Store) DeleteAdminUser(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM admin_users WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("failed to delete admin user: %w", err)
	}
	return checkAffected(res, "admin user")
}

func scanAdminUser(row rowScanner) (*AdminUser, error) {
	var (
		u           AdminUser
		enabled     int
		lastLoginAt sql.NullInt64
		createdAt   int64
		updatedAt   int64
	)
	err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &enabled,
		&lastLoginAt, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to scan admin user: %w", err)
	}
	u.Enabled = enabled != 0
	u.LastLoginAt = unixPtr(lastLoginAt)
	u.CreatedAt = time.Unix(createdAt, 0)
	u.UpdatedAt = time.Unix(updatedAt, 0)
	return &u, nil
}
