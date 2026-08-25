package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const adminSessionColumns = `id, user_id, created_at, expires_at, last_seen_at, ip, user_agent`

// CreateAdminSession records a signed-in session. ID must be the hash of the
// session token, not the token.
func (s *Store) CreateAdminSession(ctx context.Context, sess *AdminSession) error {
	if sess == nil || sess.ID == "" || sess.UserID == "" {
		return fmt.Errorf("session ID and user ID are required")
	}
	if sess.ExpiresAt.IsZero() {
		return fmt.Errorf("session expiry is required")
	}

	now := time.Now()
	sess.CreatedAt = now
	sess.LastSeenAt = now

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO admin_sessions (id, user_id, created_at, expires_at, last_seen_at, ip, user_agent)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		sess.ID, sess.UserID, now.Unix(), sess.ExpiresAt.Unix(), now.Unix(),
		sess.IP, sess.UserAgent)
	if err != nil {
		return fmt.Errorf("failed to create session: %w", err)
	}
	return nil
}

// GetAdminSession looks up a session by token hash. Expired sessions are
// reported as missing so callers cannot accidentally honour one.
func (s *Store) GetAdminSession(ctx context.Context, id string) (*AdminSession, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+adminSessionColumns+` FROM admin_sessions WHERE id = ?`, id)

	sess, err := scanAdminSession(row)
	if err != nil {
		return nil, err
	}
	if time.Now().After(sess.ExpiresAt) {
		return nil, ErrNotFound
	}
	return sess, nil
}

// TouchAdminSession records activity on a session.
func (s *Store) TouchAdminSession(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE admin_sessions SET last_seen_at = ? WHERE id = ?`, time.Now().Unix(), id)
	if err != nil {
		return fmt.Errorf("failed to touch session: %w", err)
	}
	return nil
}

// DeleteAdminSession signs one session out.
func (s *Store) DeleteAdminSession(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM admin_sessions WHERE id = ?`, id); err != nil {
		return fmt.Errorf("failed to delete session: %w", err)
	}
	return nil
}

// DeleteAdminSessionsForUser signs a user out everywhere. Called whenever a
// password changes or an account is disabled, so a stolen cookie dies with it.
func (s *Store) DeleteAdminSessionsForUser(ctx context.Context, userID string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM admin_sessions WHERE user_id = ?`, userID); err != nil {
		return fmt.Errorf("failed to delete user sessions: %w", err)
	}
	return nil
}

// PurgeExpiredAdminSessions removes sessions past their expiry.
func (s *Store) PurgeExpiredAdminSessions(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM admin_sessions WHERE expires_at <= ?`, time.Now().Unix())
	if err != nil {
		return 0, fmt.Errorf("failed to purge expired sessions: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil
	}
	return n, nil
}

func scanAdminSession(row rowScanner) (*AdminSession, error) {
	var (
		sess       AdminSession
		createdAt  int64
		expiresAt  int64
		lastSeenAt int64
	)
	err := row.Scan(&sess.ID, &sess.UserID, &createdAt, &expiresAt, &lastSeenAt,
		&sess.IP, &sess.UserAgent)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to scan session: %w", err)
	}
	sess.CreatedAt = time.Unix(createdAt, 0)
	sess.ExpiresAt = time.Unix(expiresAt, 0)
	sess.LastSeenAt = time.Unix(lastSeenAt, 0)
	return &sess, nil
}
