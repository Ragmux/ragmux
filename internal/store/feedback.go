package store

import (
	"context"
	"errors"
	"time"
)

// ErrInvalidFeedbackScore is returned for a score other than 1 or -1.
var ErrInvalidFeedbackScore = errors.New("store: feedback score must be 1 or -1")

// Bounds on the feedback summary. The label breakdown is cut to
// MaxFeedbackLabels so one project that sends a new label every time cannot
// grow the answer with its traffic.
const (
	DefaultFeedbackDays = 30
	MaxFeedbackDays     = 365
	MaxFeedbackLabels   = 50
)

// UpsertFeedback records a score (1 or -1) and an optional label for the
// request_id of a project (ADR-042). It reports ErrNotFound when the project
// has no request log row with that request_id; the check and the write are one
// statement, so a row deleted in between cannot leave feedback behind.
// A second call for the same pair replaces the score, the label and the key
// (the last writer wins) and keeps created_at.
func (s *Store) UpsertFeedback(ctx context.Context, projectID int64, requestID string, score int, label *string, apiKeyID *int64) error {
	var column int16
	switch score {
	case 1:
		column = 1
	case -1:
		column = -1
	default:
		return ErrInvalidFeedbackScore
	}
	tag, err := s.pool.Exec(ctx, `INSERT INTO request_feedback (project_id, request_id, score, label, api_key_id)
		SELECT $1, $2, $3, $4, $5
		WHERE EXISTS (SELECT 1 FROM request_logs WHERE project_id = $1 AND request_id = $2)
		ON CONFLICT (project_id, request_id) DO UPDATE
		SET score = EXCLUDED.score, label = EXCLUDED.label, api_key_id = EXCLUDED.api_key_id, updated_at = now()`,
		projectID, requestID, column, label, apiKeyID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// FeedbackLabelCount is one label's share of the feedback in a window.
type FeedbackLabelCount struct {
	Label    string `json:"label"`
	Positive int64  `json:"positive"`
	Negative int64  `json:"negative"`
}

// FeedbackDay is one UTC day of the feedback series.
type FeedbackDay struct {
	Day      string `json:"day"`
	Positive int64  `json:"positive"`
	Negative int64  `json:"negative"`
}

// FeedbackSummary is a project's feedback over a window of days.
type FeedbackSummary struct {
	Days     int   `json:"days"`
	Positive int64 `json:"positive"`
	Negative int64 `json:"negative"`
	Total    int64 `json:"total"`
	// Ratio is positive / total; nil when there is no feedback in the window.
	Ratio *float64 `json:"ratio"`
	// Labels is the breakdown of the labelled feedback, the busiest labels
	// first; feedback sent without a label is in the totals only.
	Labels []FeedbackLabelCount `json:"labels"`
	// Daily lists the days that have feedback, oldest first.
	Daily []FeedbackDay `json:"daily"`
}

// FeedbackSummary counts a project's feedback created in the last days days
// (default DefaultFeedbackDays, capped at MaxFeedbackDays), in total, by label
// and by UTC day.
func (s *Store) FeedbackSummary(ctx context.Context, projectID int64, days int) (*FeedbackSummary, error) {
	if days <= 0 {
		days = DefaultFeedbackDays
	}
	if days > MaxFeedbackDays {
		days = MaxFeedbackDays
	}
	since := time.Now().UTC().AddDate(0, 0, -days+1).Truncate(24 * time.Hour)
	out := &FeedbackSummary{Days: days, Labels: []FeedbackLabelCount{}, Daily: []FeedbackDay{}}

	if err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FILTER (WHERE score > 0), COUNT(*) FILTER (WHERE score < 0)
		FROM request_feedback WHERE project_id = $1 AND created_at >= $2`, projectID, since).
		Scan(&out.Positive, &out.Negative); err != nil {
		return nil, err
	}
	out.Total = out.Positive + out.Negative
	if out.Total > 0 {
		r := float64(out.Positive) / float64(out.Total)
		out.Ratio = &r
	}

	rows, err := s.pool.Query(ctx, `SELECT label, COUNT(*) FILTER (WHERE score > 0), COUNT(*) FILTER (WHERE score < 0)
		FROM request_feedback WHERE project_id = $1 AND created_at >= $2 AND label IS NOT NULL
		GROUP BY label ORDER BY COUNT(*) DESC, label LIMIT $3`, projectID, since, MaxFeedbackLabels)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var l FeedbackLabelCount
		if err := rows.Scan(&l.Label, &l.Positive, &l.Negative); err != nil {
			rows.Close()
			return nil, err
		}
		out.Labels = append(out.Labels, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = s.pool.Query(ctx, `SELECT date_trunc('day', created_at AT TIME ZONE 'UTC'),
		COUNT(*) FILTER (WHERE score > 0), COUNT(*) FILTER (WHERE score < 0)
		FROM request_feedback WHERE project_id = $1 AND created_at >= $2 GROUP BY 1 ORDER BY 1`, projectID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var d FeedbackDay
		var day time.Time
		if err := rows.Scan(&day, &d.Positive, &d.Negative); err != nil {
			return nil, err
		}
		d.Day = day.Format("2006-01-02")
		out.Daily = append(out.Daily, d)
	}
	return out, rows.Err()
}
