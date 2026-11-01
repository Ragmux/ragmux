package store

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// Bounds on the session and tool columns of request_logs. The values reach the
// row from a client header (the session) and from the model (the tool names),
// so without a bound one request could leave a row as large as its body.
const (
	// MaxSessionIDBytes is the longest X-Ragmux-Session value (ADR-032).
	MaxSessionIDBytes = 128
	// MaxToolNames is how many distinct tool names one row keeps.
	MaxToolNames = 32
	// MaxToolNameBytes is the longest tool name a row keeps.
	MaxToolNameBytes = 64
)

// clampToolNames makes the tools a model called fit request_logs.tool_names:
// each name loses NUL bytes (PostgreSQL text cannot hold them) and is cut to
// MaxToolNameBytes on a character boundary, then the set is de-duplicated and
// sorted and cut to MaxToolNames. It returns nil for no names, which the
// column stores as NULL.
func clampToolNames(names []string) []string {
	seen := make(map[string]struct{}, len(names))
	out := make([]string, 0, len(names))
	for _, n := range names {
		if strings.IndexByte(n, 0) >= 0 {
			n = strings.ReplaceAll(n, "\x00", "")
		}
		n = cutBytes(n, MaxToolNameBytes)
		if n == "" {
			continue
		}
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	if len(out) > MaxToolNames {
		out = out[:MaxToolNames]
	}
	return out
}

// cutBytes cuts s to at most n bytes without splitting a character.
func cutBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// clampSessionID is the session id as the row keeps it. The gateway only lets
// a valid id through, so this is the bound a writer other than the gateway
// cannot talk its way around.
func clampSessionID(s string) string {
	if strings.IndexByte(s, 0) >= 0 {
		s = strings.ReplaceAll(s, "\x00", "")
	}
	return cutBytes(s, MaxSessionIDBytes)
}

// SessionUsage is one session's running totals: what the session cap reads and
// the admin session list shows (ADR-032).
type SessionUsage struct {
	ProjectID        int64  `json:"project_id"`
	SessionID        string `json:"session_id"`
	Requests         int64  `json:"requests"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	// Tokens is PromptTokens + CompletionTokens, the figure the cap compares.
	Tokens    int64  `json:"tokens"`
	FirstSeen string `json:"first_seen"`
	LastSeen  string `json:"last_seen"`
}

// GetSessionUsage reads a session's totals with a single-row lookup. A session
// that has no row yet reads as zeros.
func (s *Store) GetSessionUsage(ctx context.Context, projectID int64, sessionID string) (SessionUsage, error) {
	u := SessionUsage{ProjectID: projectID, SessionID: sessionID}
	err := s.pool.QueryRow(ctx, `SELECT requests, prompt_tokens, completion_tokens FROM session_usage
		WHERE project_id = $1 AND session_id = $2`, projectID, sessionID).
		Scan(&u.Requests, &u.PromptTokens, &u.CompletionTokens)
	if errors.Is(err, pgx.ErrNoRows) {
		return u, nil
	}
	if err != nil {
		return u, err
	}
	u.Tokens = u.PromptTokens + u.CompletionTokens
	return u, nil
}

// RecordSessionUsage adds one finished request to a session's totals,
// creating the row on the session's first request. Negative counts are
// floored at zero: a total must never go down.
func (s *Store) RecordSessionUsage(ctx context.Context, projectID int64, sessionID string, promptTokens, completionTokens int) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO session_usage (project_id, session_id, requests, prompt_tokens, completion_tokens)
		VALUES ($1, $2, 1, $3, $4)
		ON CONFLICT (project_id, session_id) DO UPDATE SET
			requests = session_usage.requests + 1,
			prompt_tokens = session_usage.prompt_tokens + EXCLUDED.prompt_tokens,
			completion_tokens = session_usage.completion_tokens + EXCLUDED.completion_tokens,
			last_seen = now()`,
		projectID, clampSessionID(sessionID), int64(nonNegative(promptTokens)), int64(nonNegative(completionTokens)))
	return err
}

func nonNegative(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

// Bounds of the session detail's request rows.
const (
	DefaultSessionRequests = 100
	MaxSessionRequests     = 1000
)

// Bounds of the session list.
const (
	DefaultSessionLimit = 50
	MaxSessionLimit     = 500
)

// ListSessions returns a project's sessions, the most recently active first.
// limit is DefaultSessionLimit when not positive and capped at
// MaxSessionLimit; offset skips that many sessions.
func (s *Store) ListSessions(ctx context.Context, projectID int64, limit, offset int) ([]*SessionUsage, error) {
	if limit <= 0 {
		limit = DefaultSessionLimit
	}
	if limit > MaxSessionLimit {
		limit = MaxSessionLimit
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.pool.Query(ctx, `SELECT session_id, requests, prompt_tokens, completion_tokens, first_seen, last_seen
		FROM session_usage WHERE project_id = $1 ORDER BY last_seen DESC, session_id LIMIT $2 OFFSET $3`,
		projectID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*SessionUsage{}
	for rows.Next() {
		u := &SessionUsage{ProjectID: projectID}
		var first, last time.Time
		if err := rows.Scan(&u.SessionID, &u.Requests, &u.PromptTokens, &u.CompletionTokens, &first, &last); err != nil {
			return nil, err
		}
		u.Tokens = u.PromptTokens + u.CompletionTokens
		u.FirstSeen, u.LastSeen = ts(first), ts(last)
		out = append(out, u)
	}
	return out, rows.Err()
}

// GetSession returns one session's totals, ErrNotFound when it has no row.
func (s *Store) GetSession(ctx context.Context, projectID int64, sessionID string) (*SessionUsage, error) {
	u := &SessionUsage{ProjectID: projectID, SessionID: sessionID}
	var first, last time.Time
	err := s.pool.QueryRow(ctx, `SELECT requests, prompt_tokens, completion_tokens, first_seen, last_seen
		FROM session_usage WHERE project_id = $1 AND session_id = $2`, projectID, sessionID).
		Scan(&u.Requests, &u.PromptTokens, &u.CompletionTokens, &first, &last)
	if err != nil {
		return nil, scanErr(err)
	}
	u.Tokens = u.PromptTokens + u.CompletionTokens
	u.FirstSeen, u.LastSeen = ts(first), ts(last)
	return u, nil
}

// SessionRequests returns the request log rows of one session, newest first.
// limit is DefaultSessionRequests when not positive and capped at
// MaxSessionRequests, the way ListSessions caps its own.
func (s *Store) SessionRequests(ctx context.Context, projectID int64, sessionID string, limit int) ([]*RequestLog, error) {
	if limit <= 0 {
		limit = DefaultSessionRequests
	}
	if limit > MaxSessionRequests {
		limit = MaxSessionRequests
	}
	rows, err := s.pool.Query(ctx, `SELECT `+requestLogCols+` FROM request_logs
		WHERE project_id = $1 AND session_id = $2 ORDER BY created_at DESC, id DESC LIMIT $3`,
		projectID, sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return s.collectRequestLogs(rows, "session_requests")
}
