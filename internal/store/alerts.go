package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// LockAlerts is the leader lock the limit-alert job takes each round so one
// replica evaluates at a time. Exported: internal/alerts takes it.
const LockAlerts int64 = 0x7261676d75780004 // "ragmux" + 4

// Subject kinds and limit kinds of alert_events. The limit kinds are a closed
// set (the table has a CHECK on them).
const (
	AlertSubjectProject = "project"
	AlertSubjectKey     = "key"
	AlertSubjectEndUser = "end_user"

	AlertLimitDailyTokens   = "budget_daily_tokens"
	AlertLimitMonthlyTokens = "budget_monthly_tokens"
	AlertLimitMonthlyUSD    = "budget_monthly_usd"
	AlertLimitUnpriced      = "unpriced"
)

// AlertSettings is a project's limit-alert configuration (project_alert_settings).
// A project with no row has alerts off and no money budget.
type AlertSettings struct {
	ProjectID int64
	Enabled   bool
	// BudgetMonthlyUSDMicros is the monthly money budget in USD millionths;
	// 0 means none. It only ever warns.
	BudgetMonthlyUSDMicros int64
	UpdatedAt              time.Time
}

// GetAlertSettings returns the project's settings, or the zero settings (off,
// no budget) when it has none yet. UpdatedAt is the zero time then.
func (s *Store) GetAlertSettings(ctx context.Context, projectID int64) (AlertSettings, error) {
	out := AlertSettings{ProjectID: projectID}
	err := s.pool.QueryRow(ctx, `SELECT enabled, budget_monthly_usd_micros, updated_at
		FROM project_alert_settings WHERE project_id = $1`, projectID).
		Scan(&out.Enabled, &out.BudgetMonthlyUSDMicros, &out.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, nil
	}
	return out, err
}

// SetAlertSettings stores the project's settings, creating the row on first
// use. A negative budget is refused with ErrInvalidAlertSettings.
func (s *Store) SetAlertSettings(ctx context.Context, projectID int64, enabled bool, budgetMonthlyUSDMicros int64) (AlertSettings, error) {
	if budgetMonthlyUSDMicros < 0 {
		return AlertSettings{}, fmt.Errorf("%w: the budget cannot be negative", ErrInvalidAlertSettings)
	}
	out := AlertSettings{ProjectID: projectID}
	err := s.pool.QueryRow(ctx, `INSERT INTO project_alert_settings (project_id, enabled, budget_monthly_usd_micros)
		VALUES ($1, $2, $3)
		ON CONFLICT (project_id) DO UPDATE SET enabled = EXCLUDED.enabled,
			budget_monthly_usd_micros = EXCLUDED.budget_monthly_usd_micros, updated_at = now()
		RETURNING enabled, budget_monthly_usd_micros, updated_at`, projectID, enabled, budgetMonthlyUSDMicros).
		Scan(&out.Enabled, &out.BudgetMonthlyUSDMicros, &out.UpdatedAt)
	return out, err
}

// ErrInvalidAlertSettings is returned for settings the schema would refuse.
var ErrInvalidAlertSettings = errors.New("invalid alert settings")

// AlertTarget is a project the alert job evaluates: alerts are on and its
// alerts endpoint is registered and not disabled. Only these projects are
// looked at, so a deployment that uses no alerts pays for no scan.
type AlertTarget struct {
	ProjectID   int64
	ProjectName string
	EndpointID  int64
	// BudgetMonthlyUSDMicros is 0 when the project sets no money budget.
	BudgetMonthlyUSDMicros int64
	// EndUserDailyTokens and EndUserMonthlyTokens are the per-end-user token
	// ceilings (0 = none); alerts for end users fire only when one is set.
	EndUserDailyTokens, EndUserMonthlyTokens int64
	// SettingsUpdatedAt is when the project's alert settings (on or off, money
	// budget) last changed; ProjectUpdatedAt when the project row did, which is
	// when its token budgets and end-user ceilings last may have. The job uses
	// them to tell a limit that was in force in a closed window from one set
	// after it.
	SettingsUpdatedAt, ProjectUpdatedAt time.Time
}

// AlertTargets lists the projects to evaluate, in id order.
func (s *Store) AlertTargets(ctx context.Context) ([]AlertTarget, error) {
	rows, err := s.pool.Query(ctx, `SELECT p.id, p.name, e.id, a.budget_monthly_usd_micros,
			p.end_user_daily_tokens, p.end_user_monthly_tokens, a.updated_at, p.updated_at
		FROM project_alert_settings a
		JOIN projects p ON p.id = a.project_id
		JOIN webhook_endpoints e ON e.project_id = p.id AND e.purpose = 'alerts' AND e.disabled_at IS NULL
		WHERE a.enabled
		ORDER BY p.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AlertTarget
	for rows.Next() {
		var t AlertTarget
		if err := rows.Scan(&t.ProjectID, &t.ProjectName, &t.EndpointID, &t.BudgetMonthlyUSDMicros,
			&t.EndUserDailyTokens, &t.EndUserMonthlyTokens, &t.SettingsUpdatedAt, &t.ProjectUpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// EndUserOverCap is an end user whose tokens in the current day or month have
// reached the project's per-end-user ceiling for that window.
type EndUserOverCap struct {
	ProjectID   int64
	EndUser     string
	Period      string // PeriodDay or PeriodMonth
	PeriodStart time.Time
	Used, Cap   int64
}

// EndUsersOverCap returns, among the given projects, the end users at or over
// their project's day or month ceiling in the windows w. Only projects that
// set a ceiling can match, and only rows of the current window are read, so
// the result is the handful of end users that crossed it, not every end user.
func (s *Store) EndUsersOverCap(ctx context.Context, w UsageWindows, projectIDs []int64) ([]EndUserOverCap, error) {
	if len(projectIDs) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT u.project_id, u.end_user, u.period, u.period_start,
			u.prompt_tokens + u.completion_tokens,
			CASE u.period WHEN 'day' THEN p.end_user_daily_tokens ELSE p.end_user_monthly_tokens END
		FROM end_user_usage u
		JOIN projects p ON p.id = u.project_id
		WHERE u.project_id = ANY($3) AND (
			(u.period = 'day' AND u.period_start = $1 AND p.end_user_daily_tokens > 0
				AND u.prompt_tokens + u.completion_tokens >= p.end_user_daily_tokens)
			OR (u.period = 'month' AND u.period_start = $2 AND p.end_user_monthly_tokens > 0
				AND u.prompt_tokens + u.completion_tokens >= p.end_user_monthly_tokens))
		ORDER BY u.project_id, u.period, u.end_user`, w.Day, w.Month, projectIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EndUserOverCap
	for rows.Next() {
		var o EndUserOverCap
		if err := rows.Scan(&o.ProjectID, &o.EndUser, &o.Period, &o.PeriodStart, &o.Used, &o.Cap); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// AlertEvent is the identity of one alert: what the UNIQUE key of alert_events
// is made of. The same event is never recorded twice.
type AlertEvent struct {
	ProjectID   int64
	SubjectKind string
	SubjectID   string
	LimitKind   string
	Threshold   int
	WindowStart time.Time
	// LimitValue is the limit the event was measured against, so raising the
	// limit and crossing it again is a new event.
	LimitValue int64
}

// AlertPayload builds the delivery of a recorded event. It receives the id and
// the creation time the database gave the row (the delivery's dedupe key is
// that id) and returns the event type and the JSON body.
type AlertPayload func(id int64, createdAt time.Time) (eventType string, body []byte, err error)

// RecordAlertEvent records the event and queues its delivery to the endpoint
// in one transaction, and reports whether it did. It reports false, and writes
// nothing, when the event was recorded before or when the endpoint is gone or
// disabled: in that case the event is not remembered, so it is sent if the
// endpoint comes back while the limit is still crossed. Recording without
// queueing is impossible by construction; an alert that was recorded but never
// queued would be lost for good.
func (s *Store) RecordAlertEvent(ctx context.Context, e AlertEvent, endpointID int64, build AlertPayload) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful commit
	var (
		id      int64
		created time.Time
	)
	err = tx.QueryRow(ctx, `INSERT INTO alert_events
			(project_id, subject_kind, subject_id, limit_kind, threshold, window_start, limit_value)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (project_id, subject_kind, subject_id, limit_kind, threshold, window_start, limit_value) DO NOTHING
		RETURNING id, created_at`,
		e.ProjectID, e.SubjectKind, e.SubjectID, e.LimitKind, e.Threshold, e.WindowStart, e.LimitValue).Scan(&id, &created)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	eventType, body, err := build(id, created)
	if err != nil {
		return false, err
	}
	// The same statement as EnqueueWebhookDelivery, inside this transaction.
	tag, err := tx.Exec(ctx, `INSERT INTO webhook_deliveries (endpoint_id, event_type, dedupe_key, payload)
		SELECT id, $2::text, $3::text, $4::jsonb FROM webhook_endpoints WHERE id = $1 AND disabled_at IS NULL
		ON CONFLICT (endpoint_id, dedupe_key) DO NOTHING`, endpointID, eventType, strconv.FormatInt(id, 10), string(body))
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() != 1 {
		return false, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// AlertEventKeys lists the identity of every alert event of the given projects
// whose window began at or after since. The job reads it once a round to skip
// the events it already recorded without opening a transaction for each; the
// UNIQUE key stays what actually keeps an event once.
func (s *Store) AlertEventKeys(ctx context.Context, projectIDs []int64, since time.Time) ([]AlertEvent, error) {
	if len(projectIDs) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT project_id, subject_kind, subject_id, limit_kind, threshold, window_start, limit_value
		FROM alert_events WHERE project_id = ANY($1) AND window_start >= $2`, projectIDs, since.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AlertEvent
	for rows.Next() {
		var e AlertEvent
		if err := rows.Scan(&e.ProjectID, &e.SubjectKind, &e.SubjectID, &e.LimitKind, &e.Threshold, &e.WindowStart, &e.LimitValue); err != nil {
			return nil, err
		}
		e.WindowStart = e.WindowStart.UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

// WebhookEndpointMeta is an endpoint without its secrets. The admin API reads
// and edits endpoints through it so that a row whose secret this replica cannot
// open (a damaged row, a wrong SECRET_KEY) can still be shown, re-pointed,
// rotated and deleted: opening the secret is only needed to sign.
type WebhookEndpointMeta struct {
	ID            int64
	ProjectID     int64
	Purpose       string
	URL           string
	DisabledAt    *time.Time
	PrevExpiresAt *time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Meta is the endpoint without its secrets.
func (e *WebhookEndpoint) Meta() WebhookEndpointMeta {
	return WebhookEndpointMeta{ID: e.ID, ProjectID: e.ProjectID, Purpose: e.Purpose, URL: e.URL,
		DisabledAt: e.DisabledAt, PrevExpiresAt: e.PrevExpiresAt, CreatedAt: e.CreatedAt, UpdatedAt: e.UpdatedAt}
}

const webhookMetaCols = "id, project_id, purpose, url, disabled_at, prev_expires_at, created_at, updated_at"

func scanWebhookMeta(row pgx.Row) (*WebhookEndpointMeta, error) {
	var m WebhookEndpointMeta
	if err := row.Scan(&m.ID, &m.ProjectID, &m.Purpose, &m.URL, &m.DisabledAt, &m.PrevExpiresAt, &m.CreatedAt, &m.UpdatedAt); err != nil {
		return nil, scanErr(err)
	}
	return &m, nil
}

// GetWebhookEndpointMeta returns the project's endpoint for a purpose without
// opening its secrets, or ErrNotFound.
func (s *Store) GetWebhookEndpointMeta(ctx context.Context, projectID int64, purpose string) (*WebhookEndpointMeta, error) {
	return scanWebhookMeta(s.pool.QueryRow(ctx,
		"SELECT "+webhookMetaCols+" FROM webhook_endpoints WHERE project_id = $1 AND purpose = $2", projectID, purpose))
}

// SetWebhookEndpointURL points an endpoint at a new URL and opens it again if a
// 410 had closed it; the secret is kept and not read. Like
// UpdateWebhookEndpointURL, but it works on a row whose secret cannot be opened.
func (s *Store) SetWebhookEndpointURL(ctx context.Context, id int64, url string) (*WebhookEndpointMeta, error) {
	if url == "" || len(url) > 2048 {
		return nil, fmt.Errorf("%w: url must be 1 to 2048 bytes", ErrInvalidWebhook)
	}
	return scanWebhookMeta(s.pool.QueryRow(ctx, `UPDATE webhook_endpoints
		SET url = $2, disabled_at = NULL, updated_at = now() WHERE id = $1 RETURNING `+webhookMetaCols, id, url))
}

// forgetUnsentAlerts deletes the alert events whose delivery to the endpoint
// was never delivered (still pending, or failed). The event is what makes an
// alert once-only; a receiver that never got it must get it when the endpoint
// is open again, and the next round does that for a limit that is still
// crossed. A delivered event stays, so a receiver is not told twice.
func forgetUnsentAlerts(ctx context.Context, tx pgx.Tx, endpointID int64) error {
	_, err := tx.Exec(ctx, `DELETE FROM alert_events e USING webhook_deliveries d
		WHERE d.endpoint_id = $1 AND d.dedupe_key = e.id::text AND d.status <> $2`, endpointID, DeliveryDelivered)
	return err
}

// DeleteWebhookEndpointForgettingUnsent deletes an endpoint (and its
// deliveries, by cascade) and forgets the alert events it had not delivered,
// in one transaction, so registering the endpoint again brings a standing
// breach back. ErrNotFound for an unknown id.
func (s *Store) DeleteWebhookEndpointForgettingUnsent(ctx context.Context, id int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful commit
	if err := forgetUnsentAlerts(ctx, tx, id); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, "DELETE FROM webhook_endpoints WHERE id = $1", id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return tx.Commit(ctx)
}

// DisableWebhookEndpointAudited closes an endpoint the way DisableWebhookEndpoint
// does (the deliveries still waiting for it fail) and, in the same transaction,
// forgets its unsent alert events and writes entry to the audit trail with the
// project and purpose added to its details. It reports whether this call closed
// it: an endpoint that was already closed gets no second audit row, however many
// deliveries or replicas report the same 410 at once, because the row is
// written only by the call whose UPDATE found disabled_at NULL. ErrNotFound for
// an unknown id.
func (s *Store) DisableWebhookEndpointAudited(ctx context.Context, id int64, entry *AuditLog) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful commit
	var (
		projectID int64
		purpose   string
	)
	closedNow := true
	err = tx.QueryRow(ctx, `UPDATE webhook_endpoints SET disabled_at = now(), updated_at = now()
		WHERE id = $1 AND disabled_at IS NULL RETURNING project_id, purpose`, id).Scan(&projectID, &purpose)
	if errors.Is(err, pgx.ErrNoRows) {
		closedNow = false
		var exists bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM webhook_endpoints WHERE id = $1)", id).Scan(&exists); err != nil {
			return false, err
		}
		if !exists {
			return false, ErrNotFound
		}
	} else if err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE webhook_deliveries SET status = $2
		WHERE endpoint_id = $1 AND status = $3`, id, DeliveryFailed, DeliveryPending); err != nil {
		return false, err
	}
	if err := forgetUnsentAlerts(ctx, tx, id); err != nil {
		return false, err
	}
	if closedNow {
		details := map[string]any{}
		for k, v := range entry.Details {
			details[k] = v
		}
		details["project_id"], details["purpose"] = projectID, purpose
		raw, err := json.Marshal(details)
		if err != nil {
			return false, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO audit_logs
			(actor_user_id, actor_username, action, target_type, target_id, details, ip)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			entry.ActorUserID, entry.ActorUsername, entry.Action, entry.TargetType, entry.TargetID, raw, entry.IP); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return closedNow, nil
}
