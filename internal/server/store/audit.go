package store

import (
	"context"
	"fmt"
	"time"
)

// Audit actor types.
const (
	ActorAdmin  = "admin"
	ActorClient = "client"
	ActorSystem = "system"
)

// AppendAudit records a mutating action.
//
// Audit writes must never fail an operation that already succeeded, so callers
// log the error and carry on rather than surfacing it.
func (s *Store) AppendAudit(ctx context.Context, entry *AuditEntry) error {
	if entry == nil || entry.Action == "" {
		return fmt.Errorf("audit action is required")
	}

	at := entry.At
	if at.IsZero() {
		at = time.Now()
	}

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audit_log (at, actor_type, actor_id, action, target_type, target_id, detail, ip)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		at.Unix(), entry.ActorType, entry.ActorID, entry.Action,
		entry.TargetType, entry.TargetID, entry.Detail, entry.IP)
	if err != nil {
		return fmt.Errorf("failed to append audit entry: %w", err)
	}
	return nil
}

// ListAudit returns the most recent entries, newest first.
func (s *Store) ListAudit(ctx context.Context, limit int) ([]*AuditEntry, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, at, actor_type, actor_id, action, target_type, target_id, detail, ip
		 FROM audit_log ORDER BY at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to list audit entries: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*AuditEntry
	for rows.Next() {
		var (
			entry AuditEntry
			at    int64
		)
		if err := rows.Scan(&entry.ID, &at, &entry.ActorType, &entry.ActorID,
			&entry.Action, &entry.TargetType, &entry.TargetID, &entry.Detail, &entry.IP); err != nil {
			return nil, fmt.Errorf("failed to scan audit entry: %w", err)
		}
		entry.At = time.Unix(at, 0)
		out = append(out, &entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to read audit entries: %w", err)
	}
	return out, nil
}
