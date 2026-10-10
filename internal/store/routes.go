package store

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Route kinds and strategies (ADR-017). Only chat routes are resolved by the
// gateway today; the other kinds are stored so a later endpoint does not need
// a schema change.
const (
	RouteKindChat       = "chat"
	RouteKindEmbeddings = "embeddings"
	RouteKindRerank     = "rerank"

	RouteStrategyWeighted = "weighted"
)

// Bounds on a route, enforced here and by CHECK constraints in migration 0019.
const (
	MaxRouteNameLen   = 256
	MaxRouteTargets   = 8
	MinTargetWeight   = 1
	MaxTargetWeight   = 1000
	MaxTargetPriority = 100
)

// IsValidRouteKind reports whether k is a known route kind.
func IsValidRouteKind(k string) bool {
	return k == RouteKindChat || k == RouteKindEmbeddings || k == RouteKindRerank
}

// RouteTarget is one connection behind a route.
type RouteTarget struct {
	ID           int64 `json:"id"`
	ConnectionID int64 `json:"connection_id"`
	// ConnectionName and ProviderType are filled when a route is read, for
	// display; they are ignored on write.
	ConnectionName string `json:"connection_name"`
	ProviderType   string `json:"provider_type"`
	// Model, when set, replaces the connection's model_name for requests sent
	// through this target; nil keeps the connection's own.
	Model    *string `json:"model"`
	Weight   int     `json:"weight"`
	Priority int     `json:"priority"`
	// Connection is the full connection with its decrypted key. It is loaded
	// only by ResolveProjectRoute and never serialised.
	Connection *ModelConnection `json:"-"`
}

// Route is a named set of targets inside a project.
type Route struct {
	ID        int64         `json:"id"`
	ProjectID int64         `json:"project_id"`
	Name      string        `json:"name"`
	Kind      string        `json:"kind"`
	Strategy  string        `json:"strategy"`
	Targets   []RouteTarget `json:"targets"`
	CreatedAt string        `json:"created_at"`
	UpdatedAt string        `json:"updated_at"`
}

// routeQuerier is what pgx.Tx and the pool share for reads.
type routeQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// leadRow prepends fixed destinations to a row scan, so scanConn can read the
// connection columns of a joined row that starts with route and target ones.
type leadRow struct {
	rows pgx.Rows
	lead []any
}

func (l leadRow) Scan(dest ...any) error {
	return l.rows.Scan(append(append([]any{}, l.lead...), dest...)...)
}

// qualifiedConnCols is connCols with every column prefixed by the alias.
func qualifiedConnCols(alias string) string {
	parts := strings.Split(connCols, ",")
	for i, p := range parts {
		parts[i] = alias + "." + strings.TrimSpace(p)
	}
	return strings.Join(parts, ", ")
}

const routeSelectCols = `r.id, r.project_id, r.name, r.kind, r.strategy, r.created_at, r.updated_at,
	t.id, t.connection_id, t.model, t.weight, t.priority`

// loadRoutes reads routes with their targets for one project. withConn also
// loads each target's connection with its decrypted key; otherwise only the
// connection's name and provider type are read. The kind and route id filters
// are skipped when empty or zero. Targets come back ordered by priority, then
// id.
func (s *Store) loadRoutes(ctx context.Context, q routeQuerier, projectID int64, kind string, routeID int64, withConn bool) ([]*Route, error) {
	cols := routeSelectCols + ", c.name, c.provider_type"
	if withConn {
		cols = routeSelectCols + ", " + qualifiedConnCols("c")
	}
	sql := `SELECT ` + cols + `
		FROM project_routes r
		JOIN route_targets t ON t.route_id = r.id
		JOIN model_connections c ON c.id = t.connection_id
		WHERE r.project_id = $1 AND ($2::text = '' OR r.kind = $2::text) AND ($3::bigint = 0 OR r.id = $3::bigint)
		ORDER BY r.name, r.id, t.priority, t.id`
	rows, err := q.Query(ctx, sql, projectID, kind, routeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []*Route{}
	var cur *Route
	for rows.Next() {
		var (
			r                Route
			t                RouteTarget
			created, updated time.Time
		)
		lead := []any{&r.ID, &r.ProjectID, &r.Name, &r.Kind, &r.Strategy, &created, &updated,
			&t.ID, &t.ConnectionID, &t.Model, &t.Weight, &t.Priority}
		if withConn {
			conn, err := s.scanConn(leadRow{rows: rows, lead: lead})
			if err != nil {
				return nil, err
			}
			t.Connection = conn
			t.ConnectionName, t.ProviderType = conn.Name, conn.ProviderType
		} else {
			dest := append(lead, &t.ConnectionName, &t.ProviderType)
			if err := rows.Scan(dest...); err != nil {
				return nil, err
			}
		}
		if cur == nil || cur.ID != r.ID {
			r.CreatedAt, r.UpdatedAt = ts(created), ts(updated)
			r.Targets = []RouteTarget{}
			cur = &r
			out = append(out, cur)
		}
		cur.Targets = append(cur.Targets, t)
	}
	return out, rows.Err()
}

// ListRoutes returns every route of a project with its targets, ordered by
// name. Connection credentials are not loaded.
func (s *Store) ListRoutes(ctx context.Context, projectID int64) ([]*Route, error) {
	return s.loadRoutes(ctx, s.pool, projectID, "", 0, false)
}

// GetRoute returns one route of a project, or ErrNotFound.
func (s *Store) GetRoute(ctx context.Context, projectID, routeID int64) (*Route, error) {
	list, err := s.loadRoutes(ctx, s.pool, projectID, "", routeID, false)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, ErrNotFound
	}
	return list[0], nil
}

// ResolveProjectRoute is the gateway's route lookup: one query, uncached
// (ADR-017). It reports whether the project has any route of the kind and, when
// one is named name, returns it with every target's connection (decrypted key
// included). The route is nil when the project has none of that name, which is
// also what a name that cannot be a route name (NUL byte, which PostgreSQL text
// rejects) gets. Targets and keys are read for the named route only, so a
// broken target on another route cannot fail the call.
func (s *Store) ResolveProjectRoute(ctx context.Context, projectID int64, kind, name string) (has bool, route *Route, err error) {
	if strings.IndexByte(name, 0) >= 0 {
		name = "" // route names are never empty (CHECK), so nothing matches
	}
	sql := `SELECT h.n, ` + routeSelectCols + `, ` + qualifiedConnCols("c") + `
		FROM (SELECT count(*) AS n FROM project_routes WHERE project_id = $1 AND kind = $2) h
		LEFT JOIN project_routes r ON r.project_id = $1 AND r.kind = $2 AND r.name = $3::text
		LEFT JOIN route_targets t ON t.route_id = r.id
		LEFT JOIN model_connections c ON c.id = t.connection_id
		ORDER BY t.priority, t.id`
	rows, err := s.pool.Query(ctx, sql, projectID, kind, name)
	if err != nil {
		return false, nil, err
	}
	defer rows.Close()

	const (
		routeCols    = 7
		targetCols   = 5
		connColCount = 14
		routeIDCol   = 1             // r.id, after h.n
		targetIDCol  = 1 + routeCols // t.id
	)
	for rows.Next() {
		var (
			n                int64
			r                Route
			t                RouteTarget
			created, updated time.Time
		)
		raw := rows.RawValues()
		hasRoute, hasTarget := raw[routeIDCol] != nil, raw[targetIDCol] != nil
		dest := []any{&n}
		if hasRoute {
			dest = append(dest, &r.ID, &r.ProjectID, &r.Name, &r.Kind, &r.Strategy, &created, &updated)
		} else {
			dest = appendDiscards(dest, routeCols)
		}
		if hasTarget {
			dest = append(dest, &t.ID, &t.ConnectionID, &t.Model, &t.Weight, &t.Priority)
			conn, err := s.scanConn(leadRow{rows: rows, lead: dest})
			if err != nil {
				return false, nil, err
			}
			t.Connection = conn
			t.ConnectionName, t.ProviderType = conn.Name, conn.ProviderType
		} else {
			dest = appendDiscards(dest, targetCols+connColCount)
			if err := rows.Scan(dest...); err != nil {
				return false, nil, err
			}
		}
		has = n > 0
		if !hasRoute {
			continue
		}
		if route == nil {
			r.CreatedAt, r.UpdatedAt = ts(created), ts(updated)
			r.Targets = []RouteTarget{}
			route = &r
		}
		if hasTarget {
			route.Targets = append(route.Targets, t)
		}
	}
	if err := rows.Err(); err != nil {
		return false, nil, err
	}
	return has, route, nil
}

// appendDiscards adds n scan destinations that accept any value, NULL included,
// for columns of a LEFT JOIN row whose other side is missing.
func appendDiscards(dest []any, n int) []any {
	for i := 0; i < n; i++ {
		dest = append(dest, new(any))
	}
	return dest
}

// ConnectionRouteKinds returns the distinct kinds of the routes that use the
// connection as a target, so a change of provider type can be checked against
// them.
func (s *Store) ConnectionRouteKinds(ctx context.Context, connectionID int64) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT r.kind FROM project_routes r
		JOIN route_targets t ON t.route_id = r.id WHERE t.connection_id = $1 ORDER BY r.kind`, connectionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	kinds := []string{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		kinds = append(kinds, k)
	}
	return kinds, rows.Err()
}

func insertTargets(ctx context.Context, tx pgx.Tx, routeID int64, targets []RouteTarget) error {
	for _, t := range targets {
		if _, err := tx.Exec(ctx, `INSERT INTO route_targets (route_id, connection_id, model, weight, priority)
			VALUES ($1, $2, $3, $4, $5)`, routeID, t.ConnectionID, t.Model, t.Weight, t.Priority); err != nil {
			return err
		}
	}
	return nil
}

// CreateRoute inserts a route and its targets in one transaction.
func (s *Store) CreateRoute(ctx context.Context, r *Route) (*Route, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful commit
	var id int64
	if err := tx.QueryRow(ctx, `INSERT INTO project_routes (project_id, name, kind, strategy)
		VALUES ($1, $2, $3, $4) RETURNING id`, r.ProjectID, r.Name, r.Kind, r.Strategy).Scan(&id); err != nil {
		return nil, err
	}
	if err := insertTargets(ctx, tx, id, r.Targets); err != nil {
		return nil, err
	}
	out, err := s.readOne(ctx, tx, r.ProjectID, id)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

// UpdateRoute changes a route's fields and replaces its targets as a whole,
// in one transaction. ErrNotFound when the route is not in the project.
func (s *Store) UpdateRoute(ctx context.Context, r *Route) (*Route, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	res, err := tx.Exec(ctx, `UPDATE project_routes SET name = $1, kind = $2, strategy = $3, updated_at = now()
		WHERE id = $4 AND project_id = $5`, r.Name, r.Kind, r.Strategy, r.ID, r.ProjectID)
	if err != nil {
		return nil, err
	}
	if res.RowsAffected() == 0 {
		return nil, ErrNotFound
	}
	if _, err := tx.Exec(ctx, "DELETE FROM route_targets WHERE route_id = $1", r.ID); err != nil {
		return nil, err
	}
	if err := insertTargets(ctx, tx, r.ID, r.Targets); err != nil {
		return nil, err
	}
	out, err := s.readOne(ctx, tx, r.ProjectID, r.ID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) readOne(ctx context.Context, q routeQuerier, projectID, routeID int64) (*Route, error) {
	list, err := s.loadRoutes(ctx, q, projectID, "", routeID, false)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, ErrNotFound
	}
	return list[0], nil
}

// DeleteRoute removes a route (its targets cascade). ErrNotFound when the
// route is not in the project.
func (s *Store) DeleteRoute(ctx context.Context, projectID, routeID int64) error {
	res, err := s.pool.Exec(ctx, "DELETE FROM project_routes WHERE id = $1 AND project_id = $2", routeID, projectID)
	if err != nil {
		return err
	}
	if res.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
