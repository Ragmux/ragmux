package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// AuditLog is one recorded management action.
type AuditLog struct {
	ID            int64          `json:"id"`
	ActorUserID   *int64         `json:"actor_user_id"`
	ActorUsername string         `json:"actor_username"`
	Action        string         `json:"action"`
	TargetType    string         `json:"target_type"`
	TargetID      *int64         `json:"target_id"`
	Details       map[string]any `json:"details"`
	IP            string         `json:"ip"`
	CreatedAt     string         `json:"created_at"`
}

// AuditFilter narrows ListAuditLogs.
type AuditFilter struct {
	Limit       int
	ActorUserID *int64
	Action      string
	Before      *time.Time
}

// InsertAuditLog persists an audit entry. Details must not contain secrets.
func (s *Store) InsertAuditLog(ctx context.Context, l *AuditLog) error {
	details := l.Details
	if details == nil {
		details = map[string]any{}
	}
	raw, err := json.Marshal(details)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO audit_logs
		(actor_user_id, actor_username, action, target_type, target_id, details, ip)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		l.ActorUserID, l.ActorUsername, l.Action, l.TargetType, l.TargetID, raw, l.IP)
	return err
}

// ListAuditLogs returns the newest entries matching the filter.
func (s *Store) ListAuditLogs(ctx context.Context, f AuditFilter) ([]*AuditLog, error) {
	if f.Limit <= 0 || f.Limit > 1000 {
		f.Limit = 100
	}
	q := `SELECT id, actor_user_id, actor_username, action, target_type, target_id, details, ip, created_at
		FROM audit_logs WHERE true`
	args := []any{}
	if f.ActorUserID != nil {
		args = append(args, *f.ActorUserID)
		q += fmt.Sprintf(" AND actor_user_id = $%d", len(args))
	}
	if f.Action != "" {
		args = append(args, f.Action+"%")
		q += fmt.Sprintf(" AND action LIKE $%d", len(args))
	}
	if f.Before != nil {
		args = append(args, f.Before.UTC())
		q += fmt.Sprintf(" AND created_at < $%d", len(args))
	}
	args = append(args, f.Limit)
	q += fmt.Sprintf(" ORDER BY id DESC LIMIT $%d", len(args))
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*AuditLog{}
	for rows.Next() {
		l := &AuditLog{}
		var raw []byte
		var created time.Time
		if err := rows.Scan(&l.ID, &l.ActorUserID, &l.ActorUsername, &l.Action, &l.TargetType, &l.TargetID,
			&raw, &l.IP, &created); err != nil {
			return nil, err
		}
		l.Details = map[string]any{}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &l.Details); err != nil {
				return nil, err
			}
		}
		l.CreatedAt = ts(created)
		out = append(out, l)
	}
	return out, rows.Err()
}
