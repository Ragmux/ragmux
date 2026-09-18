package store

import (
	"context"
	"time"
)

// ProjectMember is the minimal user view exposed in membership lists.
type ProjectMember struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Role     string `json:"role"`
	AddedAt  string `json:"added_at"`
}

// ListProjectMembers returns the users attached to a project.
func (s *Store) ListProjectMembers(ctx context.Context, projectID int64) ([]ProjectMember, error) {
	rows, err := s.pool.Query(ctx, `SELECT u.id, u.username, u.role, pm.added_at
		FROM project_members pm JOIN users u ON u.id = pm.user_id
		WHERE pm.project_id = $1 ORDER BY u.username`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProjectMember{}
	for rows.Next() {
		var m ProjectMember
		var added time.Time
		if err := rows.Scan(&m.ID, &m.Username, &m.Role, &added); err != nil {
			return nil, err
		}
		m.AddedAt = ts(added)
		out = append(out, m)
	}
	return out, rows.Err()
}

// SetProjectMembers replaces the membership set of a project in one
// transaction. Unknown user ids fail with a foreign key violation.
func (s *Store) SetProjectMembers(ctx context.Context, projectID int64, userIDs []int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful commit
	if _, err := tx.Exec(ctx, "DELETE FROM project_members WHERE project_id = $1", projectID); err != nil {
		return err
	}
	for _, id := range userIDs {
		if _, err := tx.Exec(ctx, `INSERT INTO project_members (project_id, user_id) VALUES ($1, $2)
			ON CONFLICT DO NOTHING`, projectID, id); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// AddProjectMember attaches a user to a project (no-op when already a member).
func (s *Store) AddProjectMember(ctx context.Context, projectID, userID int64) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO project_members (project_id, user_id) VALUES ($1, $2)
		ON CONFLICT DO NOTHING`, projectID, userID)
	return err
}

// IsProjectMember reports whether the user belongs to the project.
func (s *Store) IsProjectMember(ctx context.Context, projectID, userID int64) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM project_members WHERE project_id = $1 AND user_id = $2)", projectID, userID).Scan(&ok)
	return ok, err
}

// ListProjectsForUser lists the projects the user is a member of.
func (s *Store) ListProjectsForUser(ctx context.Context, userID int64) ([]*Project, error) {
	rows, err := s.pool.Query(ctx, "SELECT "+projCols+` FROM projects
		WHERE id IN (SELECT project_id FROM project_members WHERE user_id = $1) ORDER BY name`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectProjects(rows)
}
