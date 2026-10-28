package store_test

import (
	"context"
	"testing"

	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/internal/testdb"
)

func newRAGSettingsProject(t *testing.T) (*store.Store, *store.Project, *store.ModelConnection) {
	t.Helper()
	ctx := context.Background()
	s := testdb.Open(t)
	conn, err := s.CreateConnection(ctx, &store.ModelConnection{Name: "chat", ProviderType: "openai", ModelName: "m", APIKey: "sk-secret"})
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := s.CreateProject(ctx, &store.Project{Name: "p", ModelConnectionID: conn.ID})
	if err != nil {
		t.Fatal(err)
	}
	return s, p, conn
}

// A project written without the RAG settings gets the defaults that keep the
// behaviour it had before migration 0021.
func TestProjectRAGSettingsDefaults(t *testing.T) {
	_, p, _ := newRAGSettingsProject(t)
	if p.ContextPlacement != store.ContextPlacementSystem || p.RetrievalPolicy != store.RetrievalPolicyAlways ||
		p.QueryStrategy != store.QueryStrategyLastUser || p.HistoryTurnsForQuery != 3 || p.QueryModelConnectionID != nil {
		t.Errorf("defaults = %+v", p)
	}
}

func TestProjectRAGSettingsRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, p, conn := newRAGSettingsProject(t)
	p.ContextPlacement, p.RetrievalPolicy, p.QueryStrategy = store.ContextPlacementBeforeLastUser, store.RetrievalPolicyAuto, store.QueryStrategyCondense
	p.HistoryTurnsForQuery, p.QueryModelConnectionID = 10, &conn.ID
	got, err := s.UpdateProject(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if got.ContextPlacement != "before_last_user" || got.RetrievalPolicy != "auto" || got.QueryStrategy != "condense" ||
		got.HistoryTurnsForQuery != 10 || got.QueryModelConnectionID == nil || *got.QueryModelConnectionID != conn.ID {
		t.Errorf("round trip = %+v", got)
	}
	list, err := s.ListProjects(ctx)
	if err != nil || len(list) != 1 || list[0].ContextPlacement != "before_last_user" {
		t.Errorf("list = %+v (%v)", list, err)
	}
}

// The CHECK constraints are the last line of defence behind the admin API.
func TestProjectRAGSettingsRejectValuesOutsideTheChecks(t *testing.T) {
	ctx := context.Background()
	s, p, _ := newRAGSettingsProject(t)
	for name, mutate := range map[string]func(*store.Project){
		"placement":  func(p *store.Project) { p.ContextPlacement = "bogus" },
		"policy":     func(p *store.Project) { p.RetrievalPolicy = "bogus" },
		"strategy":   func(p *store.Project) { p.QueryStrategy = "bogus" },
		"turns low":  func(p *store.Project) { p.HistoryTurnsForQuery = -1 },
		"turns high": func(p *store.Project) { p.HistoryTurnsForQuery = 11 },
	} {
		bad := *p
		mutate(&bad)
		if _, err := s.UpdateProject(ctx, &bad); err == nil {
			t.Errorf("%s: an out-of-range value was accepted", name)
		}
	}
}

// A value this build does not know -- what a newer release writes after it
// widens a CHECK list -- reads as the default instead of failing the request.
func TestProjectRAGSettingsUnknownValueReadsAsDefault(t *testing.T) {
	ctx := context.Background()
	s, p, _ := newRAGSettingsProject(t)
	for _, stmt := range []string{
		"ALTER TABLE projects DROP CONSTRAINT projects_context_placement_check",
		"ALTER TABLE projects DROP CONSTRAINT projects_retrieval_policy_check",
		"ALTER TABLE projects DROP CONSTRAINT projects_query_strategy_check",
		"UPDATE projects SET context_placement = 'after_all', retrieval_policy = 'smart', query_strategy = 'rewrite'",
	} {
		if _, err := s.DB().Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	got, err := s.GetProject(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ContextPlacement != store.ContextPlacementSystem || got.RetrievalPolicy != store.RetrievalPolicyAlways || got.QueryStrategy != store.QueryStrategyLastUser {
		t.Errorf("unknown values were not mapped to the defaults: %+v", got)
	}

	// An update that leaves the settings unset must not write the defaults
	// over what a newer release stored.
	raw := func() (placement, policy, strategy string) {
		t.Helper()
		if err := s.DB().QueryRow(ctx, "SELECT context_placement, retrieval_policy, query_strategy FROM projects WHERE id = $1", p.ID).
			Scan(&placement, &policy, &strategy); err != nil {
			t.Fatal(err)
		}
		return
	}
	renamed := &store.Project{ID: p.ID, Name: "renamed", ModelConnectionID: p.ModelConnectionID}
	if _, err := s.UpdateProject(ctx, renamed); err != nil {
		t.Fatal(err)
	}
	if a, b, c := raw(); a != "after_all" || b != "smart" || c != "rewrite" {
		t.Errorf("an update without the settings rewrote them: %q %q %q", a, b, c)
	}
	// A setting that is given is written.
	renamed.ContextPlacement = store.ContextPlacementBeforeLastUser
	if _, err := s.UpdateProject(ctx, renamed); err != nil {
		t.Fatal(err)
	}
	if a, b, _ := raw(); a != "before_last_user" || b != "smart" {
		t.Errorf("a given setting was not written alone: %q %q", a, b)
	}
}

// query_model_connection_id is RESTRICT: the connection cannot be deleted
// while a project still names it, and the failure is the foreign-key
// violation the admin API maps to 409.
func TestQueryModelConnectionRestrictsDelete(t *testing.T) {
	ctx := context.Background()
	s, p, _ := newRAGSettingsProject(t)
	query, err := s.CreateConnection(ctx, &store.ModelConnection{Name: "query", ProviderType: "openai", ModelName: "q", APIKey: "sk-secret-q"})
	if err != nil {
		t.Fatal(err)
	}
	p.QueryModelConnectionID = &query.ID
	if _, err := s.UpdateProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteConnection(ctx, query.ID); !store.IsForeignKeyViolation(err) {
		t.Fatalf("delete of a referenced connection = %v, want a foreign-key violation", err)
	}
	p.QueryModelConnectionID = nil
	if _, err := s.UpdateProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteConnection(ctx, query.ID); err != nil {
		t.Errorf("delete after the reference was cleared: %v", err)
	}
}
