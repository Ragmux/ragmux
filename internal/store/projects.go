package store

import (
	"context"
	"crypto/rand"
	"math/big"
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
	// MemberIDs lists the users attached to the project.
	MemberIDs []int64 `json:"member_ids"`
	CreatedAt string  `json:"created_at"`
	UpdatedAt string  `json:"updated_at"`
}

const projCols = `id, name, model_connection_id, rag_store_id, api_key_prefix, system_prompt,
	rate_limit_rpm, rate_limit_tpm, budget_daily_tokens, budget_monthly_tokens, created_at, updated_at,
	ARRAY(SELECT user_id FROM project_members pm WHERE pm.project_id = projects.id ORDER BY user_id)`

func scanProject(row interface{ Scan(...any) error }) (*Project, error) {
	p := &Project{}
	var created, updated time.Time
	err := row.Scan(&p.ID, &p.Name, &p.ModelConnectionID, &p.RAGStoreID, &p.APIKeyPrefix, &p.SystemPrompt,
		&p.RateLimitRPM, &p.RateLimitTPM, &p.BudgetDailyTokens, &p.BudgetMonthlyTokens,
		&created, &updated, &p.MemberIDs)
	if err != nil {
		return nil, scanErr(err)
	}
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
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful commit
	var id int64
	err = tx.QueryRow(ctx, `INSERT INTO projects
		(name, model_connection_id, rag_store_id, api_key_hash, api_key_prefix, system_prompt,
		rate_limit_rpm, rate_limit_tpm, budget_daily_tokens, budget_monthly_tokens)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING id`,
		p.Name, p.ModelConnectionID, p.RAGStoreID, HashToken(key), keyPrefix(key), p.SystemPrompt,
		p.RateLimitRPM, p.RateLimitTPM, p.BudgetDailyTokens, p.BudgetMonthlyTokens).Scan(&id)
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
	_, err := s.pool.Exec(ctx, `UPDATE projects SET name=$1, model_connection_id=$2, rag_store_id=$3,
		system_prompt=$4, rate_limit_rpm=$5, rate_limit_tpm=$6, budget_daily_tokens=$7, budget_monthly_tokens=$8,
		updated_at=now() WHERE id=$9`,
		p.Name, p.ModelConnectionID, p.RAGStoreID, p.SystemPrompt,
		p.RateLimitRPM, p.RateLimitTPM, p.BudgetDailyTokens, p.BudgetMonthlyTokens, p.ID)
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
