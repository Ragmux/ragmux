package store

import (
	"context"
	"crypto/rand"
	"log/slog"
	"math/big"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// Project maps a client API key to a model connection and optional RAG store.
type Project struct {
	ID                int64  `json:"id"`
	Name              string `json:"name"`
	ModelConnectionID int64  `json:"model_connection_id"`
	RAGStoreID        *int64 `json:"rag_store_id"`
	APIKeyPrefix      string `json:"api_key_prefix"`
	SystemPrompt      string `json:"system_prompt"`
	// Limits; zero means unlimited. RPM/TPM are per UTC minute, budgets are
	// prompt+completion tokens per UTC day / month.
	RateLimitRPM        int   `json:"rate_limit_rpm"`
	RateLimitTPM        int   `json:"rate_limit_tpm"`
	BudgetDailyTokens   int64 `json:"budget_daily_tokens"`
	BudgetMonthlyTokens int64 `json:"budget_monthly_tokens"`
	// RAG behaviour (ADR-024). The zero value of each means "unset": the
	// project takes the default on create, and keeps whatever the row holds on
	// update. A Project built by hand therefore keeps the behaviour every
	// project had before these settings existed.
	//
	// ContextPlacement is one of the ContextPlacement* constants, and
	// RetrievalPolicy and QueryStrategy likewise; HistoryTurnsForQuery is
	// 1..MaxHistoryTurnsForQuery. QueryModelConnectionID is the connection
	// that condenses the retrieval query, nil for the first route target.
	ContextPlacement       string `json:"context_placement"`
	RetrievalPolicy        string `json:"retrieval_policy"`
	QueryStrategy          string `json:"query_strategy"`
	HistoryTurnsForQuery   int    `json:"history_turns_for_query"`
	QueryModelConnectionID *int64 `json:"query_model_connection_id"`
	// MemberIDs lists the users attached to the project.
	MemberIDs []int64 `json:"member_ids"`
	CreatedAt string  `json:"created_at"`
	UpdatedAt string  `json:"updated_at"`
}

// Values of the RAG settings columns. Each list is a CHECK constraint in
// migration 0021; later releases widen them.
const (
	ContextPlacementSystem         = "system"
	ContextPlacementBeforeLastUser = "before_last_user"

	RetrievalPolicyAlways = "always"
	RetrievalPolicyAuto   = "auto"
	RetrievalPolicyOff    = "off"

	QueryStrategyLastUser = "last_user"
	QueryStrategyCondense = "condense"

	DefaultHistoryTurnsForQuery = 3
	MaxHistoryTurnsForQuery     = 10
)

// ValidContextPlacement reports whether v is a placement this build knows.
func ValidContextPlacement(v string) bool {
	return v == ContextPlacementSystem || v == ContextPlacementBeforeLastUser
}

// ValidRetrievalPolicy reports whether v is a policy this build knows.
func ValidRetrievalPolicy(v string) bool {
	return v == RetrievalPolicyAlways || v == RetrievalPolicyAuto || v == RetrievalPolicyOff
}

// ValidQueryStrategy reports whether v is a strategy this build knows.
func ValidQueryStrategy(v string) bool {
	return v == QueryStrategyLastUser || v == QueryStrategyCondense
}

// withRAGDefaults fills the RAG settings an unset field leaves empty.
func (p *Project) withRAGDefaults() {
	if p.ContextPlacement == "" {
		p.ContextPlacement = ContextPlacementSystem
	}
	if p.RetrievalPolicy == "" {
		p.RetrievalPolicy = RetrievalPolicyAlways
	}
	if p.QueryStrategy == "" {
		p.QueryStrategy = QueryStrategyLastUser
	}
	if p.HistoryTurnsForQuery == 0 {
		p.HistoryTurnsForQuery = DefaultHistoryTurnsForQuery
	}
}

// unknownSettingLogged remembers which columns have already logged an
// unrecognised value, so a long-lived replica logs each once per process.
var unknownSettingLogged sync.Map

// readSetting returns v when known reports it valid, and def otherwise. A
// value outside the list is what a newer release wrote after widening the
// CHECK: this build serves the default instead of failing the request, and
// says so once (ADR-024, rolling update).
func readSetting(column, v, def string, known func(string) bool) string {
	if known(v) {
		return v
	}
	if _, seen := unknownSettingLogged.LoadOrStore(column, struct{}{}); !seen {
		slog.Warn("unknown project setting value; using the default", "column", column, "default", def)
	}
	return def
}

// normalizeRAGSettings maps values this build does not know to the defaults.
func (p *Project) normalizeRAGSettings() {
	p.ContextPlacement = readSetting("context_placement", p.ContextPlacement, ContextPlacementSystem, ValidContextPlacement)
	p.RetrievalPolicy = readSetting("retrieval_policy", p.RetrievalPolicy, RetrievalPolicyAlways, ValidRetrievalPolicy)
	p.QueryStrategy = readSetting("query_strategy", p.QueryStrategy, QueryStrategyLastUser, ValidQueryStrategy)
}

const projCols = `id, name, model_connection_id, rag_store_id, api_key_prefix, system_prompt,
	rate_limit_rpm, rate_limit_tpm, budget_daily_tokens, budget_monthly_tokens, created_at, updated_at,
	context_placement, retrieval_policy, query_strategy, history_turns_for_query, query_model_connection_id,
	ARRAY(SELECT user_id FROM project_members pm WHERE pm.project_id = projects.id ORDER BY user_id)`

func scanProject(row interface{ Scan(...any) error }) (*Project, error) {
	p := &Project{}
	var created, updated time.Time
	err := row.Scan(&p.ID, &p.Name, &p.ModelConnectionID, &p.RAGStoreID, &p.APIKeyPrefix, &p.SystemPrompt,
		&p.RateLimitRPM, &p.RateLimitTPM, &p.BudgetDailyTokens, &p.BudgetMonthlyTokens,
		&created, &updated, &p.ContextPlacement, &p.RetrievalPolicy, &p.QueryStrategy,
		&p.HistoryTurnsForQuery, &p.QueryModelConnectionID, &p.MemberIDs)
	if err != nil {
		return nil, scanErr(err)
	}
	p.normalizeRAGSettings()
	if p.MemberIDs == nil {
		p.MemberIDs = []int64{}
	}
	p.CreatedAt, p.UpdatedAt = ts(created), ts(updated)
	return p, nil
}

const keyAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// GenerateProjectKey produces a fresh "sk-proj-..." credential.
func GenerateProjectKey() (string, error) {
	return generateToken(ProjectKeyPrefix, keyBodyLen)
}

// GenerateSessionToken produces a random opaque session token.
func GenerateSessionToken() (string, error) {
	return generateToken("", 48)
}

func generateToken(prefix string, n int) (string, error) {
	out := make([]byte, n)
	max := big.NewInt(int64(len(keyAlphabet)))
	for i := range out {
		v, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		out[i] = keyAlphabet[v.Int64()]
	}
	return prefix + string(out), nil
}

func keyPrefix(k string) string {
	if len(k) > 15 {
		return k[:15]
	}
	return k
}

// CreateProject inserts a project and returns it plus the plaintext key,
// which is never stored and cannot be retrieved later.
func (s *Store) CreateProject(ctx context.Context, p *Project) (*Project, string, error) {
	return s.CreateProjectWithMembers(ctx, p, nil)
}

// CreateProjectWithMembers inserts a project and its member list in one
// transaction, so an unknown member id (foreign key violation) leaves no
// half-created project behind.
func (s *Store) CreateProjectWithMembers(ctx context.Context, p *Project, memberIDs []int64) (*Project, string, error) {
	key, err := GenerateProjectKey()
	if err != nil {
		return nil, "", err
	}
	p.withRAGDefaults()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful commit
	var id int64
	err = tx.QueryRow(ctx, `INSERT INTO projects
		(name, model_connection_id, rag_store_id, api_key_hash, api_key_prefix, system_prompt,
		rate_limit_rpm, rate_limit_tpm, budget_daily_tokens, budget_monthly_tokens,
		context_placement, retrieval_policy, query_strategy, history_turns_for_query, query_model_connection_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15) RETURNING id`,
		p.Name, p.ModelConnectionID, p.RAGStoreID, HashToken(key), keyPrefix(key), p.SystemPrompt,
		p.RateLimitRPM, p.RateLimitTPM, p.BudgetDailyTokens, p.BudgetMonthlyTokens,
		p.ContextPlacement, p.RetrievalPolicy, p.QueryStrategy, p.HistoryTurnsForQuery, p.QueryModelConnectionID).Scan(&id)
	if err != nil {
		return nil, "", err
	}
	for _, uid := range memberIDs {
		if _, err := tx.Exec(ctx, `INSERT INTO project_members (project_id, user_id) VALUES ($1, $2)
			ON CONFLICT DO NOTHING`, id, uid); err != nil {
			return nil, "", err
		}
	}
	out, err := scanProject(tx.QueryRow(ctx, "SELECT "+projCols+" FROM projects WHERE id = $1", id))
	if err != nil {
		return nil, "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, "", err
	}
	return out, key, nil
}

// UpdateProject changes mutable fields (not the key).
func (s *Store) UpdateProject(ctx context.Context, p *Project) (*Project, error) {
	// An unset RAG setting (empty string, 0) leaves the column as it is rather
	// than writing the default: a value this build does not know -- written by
	// a newer release -- reads as the default, and writing that read value
	// back would overwrite it. The nullif/coalesce pairs are the only place
	// that distinction survives.
	_, err := s.pool.Exec(ctx, `UPDATE projects SET name=$1, model_connection_id=$2, rag_store_id=$3,
		system_prompt=$4, rate_limit_rpm=$5, rate_limit_tpm=$6, budget_daily_tokens=$7, budget_monthly_tokens=$8,
		context_placement=COALESCE(NULLIF($9, ''), context_placement),
		retrieval_policy=COALESCE(NULLIF($10, ''), retrieval_policy),
		query_strategy=COALESCE(NULLIF($11, ''), query_strategy),
		history_turns_for_query=COALESCE(NULLIF($12::int, 0), history_turns_for_query),
		query_model_connection_id=$13, updated_at=now() WHERE id=$14`,
		p.Name, p.ModelConnectionID, p.RAGStoreID, p.SystemPrompt,
		p.RateLimitRPM, p.RateLimitTPM, p.BudgetDailyTokens, p.BudgetMonthlyTokens,
		p.ContextPlacement, p.RetrievalPolicy, p.QueryStrategy, p.HistoryTurnsForQuery, p.QueryModelConnectionID, p.ID)
	if err != nil {
		return nil, err
	}
	return s.GetProject(ctx, p.ID)
}

// RotateProjectKey issues a new key, invalidating the previous one.
func (s *Store) RotateProjectKey(ctx context.Context, id int64) (string, error) {
	key, err := GenerateProjectKey()
	if err != nil {
		return "", err
	}
	res, err := s.pool.Exec(ctx, "UPDATE projects SET api_key_hash=$1, api_key_prefix=$2, updated_at=now() WHERE id=$3",
		HashToken(key), keyPrefix(key), id)
	if err != nil {
		return "", err
	}
	if res.RowsAffected() == 0 {
		return "", ErrNotFound
	}
	return key, nil
}

// GetProject fetches one project.
func (s *Store) GetProject(ctx context.Context, id int64) (*Project, error) {
	return scanProject(s.pool.QueryRow(ctx, "SELECT "+projCols+" FROM projects WHERE id = $1", id))
}

// GetProjectByKey resolves a plaintext client key.
func (s *Store) GetProjectByKey(ctx context.Context, key string) (*Project, error) {
	return scanProject(s.pool.QueryRow(ctx, "SELECT "+projCols+" FROM projects WHERE api_key_hash = $1", HashToken(key)))
}

// ListProjects lists all projects.
func (s *Store) ListProjects(ctx context.Context) ([]*Project, error) {
	rows, err := s.pool.Query(ctx, "SELECT "+projCols+" FROM projects ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectProjects(rows)
}

func collectProjects(rows pgx.Rows) ([]*Project, error) {
	out := []*Project{}
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeleteProject removes a project and its request logs.
func (s *Store) DeleteProject(ctx context.Context, id int64) error {
	res, err := s.pool.Exec(ctx, "DELETE FROM projects WHERE id = $1", id)
	if err != nil {
		return err
	}
	if res.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
