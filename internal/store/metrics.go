package store

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"
)

// RequestLog is one proxied chat completion.
type RequestLog struct {
	ID               int64  `json:"id"`
	ProjectID        int64  `json:"project_id"`
	ModelName        string `json:"model_name"`
	StatusCode       int    `json:"status_code"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	Estimated        bool   `json:"estimated"`
	LatencyMs        int64  `json:"latency_ms"`
	Streamed         bool   `json:"streamed"`
	RAGUsed          bool   `json:"rag_used"`
	// RAGHits is the number of retrieved chunks injected into the prompt.
	RAGHits   int    `json:"rag_hits"`
	Error     string `json:"error"`
	CreatedAt string `json:"created_at"`
}

// InsertRequestLog persists a metric row.
func (s *Store) InsertRequestLog(ctx context.Context, l *RequestLog) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO request_logs
		(project_id, model_name, status_code, prompt_tokens, completion_tokens, estimated, latency_ms, streamed, rag_used, rag_hits, error)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		l.ProjectID, l.ModelName, l.StatusCode, l.PromptTokens, l.CompletionTokens, l.Estimated,
		l.LatencyMs, l.Streamed, l.RAGUsed, l.RAGHits, l.Error)
	return err
}

// DeleteRequestLogsBefore removes request logs older than t and reports how
// many rows were removed.
func (s *Store) DeleteRequestLogsBefore(ctx context.Context, t time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, "DELETE FROM request_logs WHERE created_at < $1", t.UTC())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// MetricsSummary aggregates request logs over a window.
type MetricsSummary struct {
	ProjectID        *int64  `json:"project_id,omitempty"`
	Requests         int     `json:"requests"`
	Errors           int     `json:"errors"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	AvgLatencyMs     float64 `json:"avg_latency_ms"`
	P95LatencyMs     int64   `json:"p95_latency_ms"`
	RAGRequests      int     `json:"rag_requests"`
	// RateLimited counts requests answered 429 by the gateway's limiter.
	RateLimited int `json:"rate_limited"`
}

// MetricsFilter narrows metric queries. A nil ProjectID means every project;
// a non-nil UserID restricts to projects the user is a member of.
type MetricsFilter struct {
	ProjectID *int64
	UserID    *int64
}

// where renders the filter as SQL predicates; args are appended after base.
func (f MetricsFilter) where(base []any) (string, []any) {
	return f.whereCol("project_id", base)
}

// whereCol is where with the request_logs project column spelled as col, for
// queries that join request_logs under an alias.
func (f MetricsFilter) whereCol(col string, base []any) (string, []any) {
	var sb strings.Builder
	if f.ProjectID != nil {
		base = append(base, *f.ProjectID)
		fmt.Fprintf(&sb, " AND %s = $%d", col, len(base))
	}
	if f.UserID != nil {
		base = append(base, *f.UserID)
		fmt.Fprintf(&sb, " AND %s IN (SELECT project_id FROM project_members WHERE user_id = $%d)", col, len(base))
	}
	return sb.String(), base
}

// Summarize computes totals matching the filter since the given time.
func (s *Store) Summarize(ctx context.Context, f MetricsFilter, since time.Time) (*MetricsSummary, error) {
	return s.SummarizeBetween(ctx, f, since, time.Time{})
}

// SummarizeBetween is Summarize bounded above: rows at or after until are
// excluded, which is how the previous comparison window is computed. A zero
// until means no upper bound.
func (s *Store) SummarizeBetween(ctx context.Context, f MetricsFilter, since, until time.Time) (*MetricsSummary, error) {
	q := `SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN status_code >= 400 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(prompt_tokens), 0), COALESCE(SUM(completion_tokens), 0),
		COALESCE(AVG(latency_ms), 0)::float8,
		COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY latency_ms), 0)::float8,
		COALESCE(SUM(CASE WHEN rag_used THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status_code = 429 THEN 1 ELSE 0 END), 0)
		FROM request_logs WHERE created_at >= $1`
	base := []any{since.UTC()}
	if !until.IsZero() {
		base = append(base, until.UTC())
		q += " AND created_at < $2"
	}
	cond, args := f.where(base)
	q += cond
	m := &MetricsSummary{ProjectID: f.ProjectID}
	var p95 float64
	if err := s.pool.QueryRow(ctx, q, args...).Scan(&m.Requests, &m.Errors, &m.PromptTokens,
		&m.CompletionTokens, &m.AvgLatencyMs, &p95, &m.RAGRequests, &m.RateLimited); err != nil {
		return nil, err
	}
	m.P95LatencyMs = int64(math.Round(p95))
	return m, nil
}

// RecentRequests returns the newest request logs matching the filter.
func (s *Store) RecentRequests(ctx context.Context, f MetricsFilter, limit int) ([]*RequestLog, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	q := `SELECT id, project_id, model_name, status_code, prompt_tokens, completion_tokens, estimated,
		latency_ms, streamed, rag_used, rag_hits, error, created_at FROM request_logs WHERE true`
	cond, args := f.where(nil)
	args = append(args, limit)
	q += cond + fmt.Sprintf(" ORDER BY id DESC LIMIT $%d", len(args))
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*RequestLog{}
	for rows.Next() {
		l := &RequestLog{}
		var created time.Time
		if err := rows.Scan(&l.ID, &l.ProjectID, &l.ModelName, &l.StatusCode, &l.PromptTokens, &l.CompletionTokens,
			&l.Estimated, &l.LatencyMs, &l.Streamed, &l.RAGUsed, &l.RAGHits, &l.Error, &created); err != nil {
			return nil, err
		}
		l.CreatedAt = ts(created)
		out = append(out, l)
	}
	return out, rows.Err()
}

// DailyBucket is request volume for one UTC day.
type DailyBucket struct {
	Day              string `json:"day"`
	Requests         int    `json:"requests"`
	Errors           int    `json:"errors"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
}

// MaxSeriesDays caps DailySeries; longer ranges belong in the CSV export.
const MaxSeriesDays = 90

// DailySeries returns per-day totals for the last n days (default 14,
// capped at MaxSeriesDays).
func (s *Store) DailySeries(ctx context.Context, f MetricsFilter, days int) ([]DailyBucket, error) {
	if days <= 0 {
		days = 14
	}
	if days > MaxSeriesDays {
		days = MaxSeriesDays
	}
	since := time.Now().UTC().AddDate(0, 0, -days+1).Truncate(24 * time.Hour)
	q := `SELECT date_trunc('day', created_at AT TIME ZONE 'UTC') AS day, COUNT(*),
		COALESCE(SUM(CASE WHEN status_code >= 400 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(prompt_tokens), 0), COALESCE(SUM(completion_tokens), 0)
		FROM request_logs WHERE created_at >= $1`
	cond, args := f.where([]any{since})
	q += cond + " GROUP BY day ORDER BY day"
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DailyBucket{}
	for rows.Next() {
		var b DailyBucket
		var day time.Time
		if err := rows.Scan(&day, &b.Requests, &b.Errors, &b.PromptTokens, &b.CompletionTokens); err != nil {
			return nil, err
		}
		b.Day = day.Format("2006-01-02")
		out = append(out, b)
	}
	return out, rows.Err()
}

// ProjectMetrics is one project's totals inside a window.
type ProjectMetrics struct {
	ProjectID        int64  `json:"project_id"`
	Name             string `json:"name"`
	Requests         int    `json:"requests"`
	Errors           int    `json:"errors"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	RateLimited      int    `json:"rate_limited"`
	RAGRequests      int    `json:"rag_requests"`
}

// SummarizeByProject breaks the window down per project, busiest first.
// Projects without requests in the window are not listed.
func (s *Store) SummarizeByProject(ctx context.Context, f MetricsFilter, since time.Time) ([]ProjectMetrics, error) {
	q := `SELECT l.project_id, COALESCE(p.name, ''), COUNT(*),
		COALESCE(SUM(CASE WHEN l.status_code >= 400 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(l.prompt_tokens), 0), COALESCE(SUM(l.completion_tokens), 0),
		COALESCE(SUM(CASE WHEN l.status_code = 429 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN l.rag_used THEN 1 ELSE 0 END), 0)
		FROM request_logs l LEFT JOIN projects p ON p.id = l.project_id WHERE l.created_at >= $1`
	cond, args := f.whereCol("l.project_id", []any{since.UTC()})
	q += cond + " GROUP BY l.project_id, p.name ORDER BY COUNT(*) DESC, l.project_id"
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProjectMetrics{}
	for rows.Next() {
		var m ProjectMetrics
		if err := rows.Scan(&m.ProjectID, &m.Name, &m.Requests, &m.Errors, &m.PromptTokens, &m.CompletionTokens,
			&m.RateLimited, &m.RAGRequests); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
