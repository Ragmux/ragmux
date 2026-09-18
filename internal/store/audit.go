package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
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

// AuditFilter narrows ListAuditLogs, CountAuditLogs and StreamAuditLogs.
// Since and Until bound created_at (inclusive / exclusive); Before is the
// paging cursor and, like Limit, is ignored by CountAuditLogs.
type AuditFilter struct {
	Limit       int
	ActorUserID *int64
	Action      string
	Before      *time.Time
	Since       *time.Time
	Until       *time.Time
}

// MaxAuditExport caps the rows StreamAuditLogs hands out.
const MaxAuditExport = 100000

const auditCols = "id, actor_user_id, actor_username, action, target_type, target_id, details, ip, created_at"

// where renders the filter as a WHERE clause; paging tells whether the
// Before cursor takes part (it never does for a count).
func (f AuditFilter) where(paging bool) (string, []any) {
	q := " WHERE true"
	args := []any{}
	if f.ActorUserID != nil {
		args = append(args, *f.ActorUserID)
		q += fmt.Sprintf(" AND actor_user_id = $%d", len(args))
	}
	if f.Action != "" {
		args = append(args, f.Action+"%")
		q += fmt.Sprintf(" AND action LIKE $%d", len(args))
	}
	if f.Since != nil {
		args = append(args, f.Since.UTC())
		q += fmt.Sprintf(" AND created_at >= $%d", len(args))
	}
	if f.Until != nil {
		args = append(args, f.Until.UTC())
		q += fmt.Sprintf(" AND created_at < $%d", len(args))
	}
	if paging && f.Before != nil {
		args = append(args, f.Before.UTC())
		q += fmt.Sprintf(" AND created_at < $%d", len(args))
	}
	return q, args
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

// ListAuditLogs returns the newest entries matching the filter and whether
// more entries exist beyond the limit (default 100, at most 1000).
func (s *Store) ListAuditLogs(ctx context.Context, f AuditFilter) (entries []*AuditLog, hasMore bool, err error) {
	if f.Limit <= 0 || f.Limit > 1000 {
		f.Limit = 100
	}
	where, args := f.where(true)
	args = append(args, f.Limit+1)
	rows, err := s.pool.Query(ctx, "SELECT "+auditCols+" FROM audit_logs"+where+
		fmt.Sprintf(" ORDER BY id DESC LIMIT $%d", len(args)), args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	entries = []*AuditLog{}
	for rows.Next() {
		l, err := scanAuditLog(rows)
		if err != nil {
			return nil, false, err
		}
		entries = append(entries, l)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(entries) > f.Limit {
		return entries[:f.Limit], true, nil
	}
	return entries, false, nil
}

// CountAuditLogs counts every entry matching the filter, ignoring the
// Before cursor and the limit.
func (s *Store) CountAuditLogs(ctx context.Context, f AuditFilter) (int64, error) {
	where, args := f.where(false)
	var n int64
	err := s.pool.QueryRow(ctx, "SELECT COUNT(*) FROM audit_logs"+where, args...).Scan(&n)
	return n, err
}

// StreamAuditLogs calls fn for every entry matching the filter, newest
// first, up to MaxAuditExport rows. A non-nil error from fn stops the scan
// and is returned.
func (s *Store) StreamAuditLogs(ctx context.Context, f AuditFilter, fn func(*AuditLog) error) error {
	where, args := f.where(true)
	args = append(args, MaxAuditExport)
	rows, err := s.pool.Query(ctx, "SELECT "+auditCols+" FROM audit_logs"+where+
		fmt.Sprintf(" ORDER BY id DESC LIMIT $%d", len(args)), args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		l, err := scanAuditLog(rows)
		if err != nil {
			return err
		}
		if err := fn(l); err != nil {
			return err
		}
	}
	return rows.Err()
}

func scanAuditLog(rows pgx.Rows) (*AuditLog, error) {
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
	return l, nil
}

// DeleteAuditLogsBefore removes audit entries older than t and reports how
// many rows were removed.
func (s *Store) DeleteAuditLogsBefore(ctx context.Context, t time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, "DELETE FROM audit_logs WHERE created_at < $1", t.UTC())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
