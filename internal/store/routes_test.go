package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/internal/testdb"
)

type routeFixture struct {
	s       *store.Store
	project *store.Project
	a, b    *store.ModelConnection
}

func newRouteFixture(t *testing.T) routeFixture {
	t.Helper()
	ctx := context.Background()
	s := testdb.Open(t)
	a, err := s.CreateConnection(ctx, &store.ModelConnection{Name: "conn-a", ProviderType: "openai", ModelName: "model-a", APIKey: "sk-secret-a"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateConnection(ctx, &store.ModelConnection{Name: "conn-b", ProviderType: "anthropic", ModelName: "model-b", APIKey: "sk-secret-b"})
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := s.CreateProject(ctx, &store.Project{Name: "p", ModelConnectionID: a.ID})
	if err != nil {
		t.Fatal(err)
	}
	return routeFixture{s: s, project: p, a: a, b: b}
}

func strPtr(s string) *string { return &s }

func (f routeFixture) route(name string, targets ...store.RouteTarget) *store.Route {
	return &store.Route{ProjectID: f.project.ID, Name: name, Kind: store.RouteKindChat,
		Strategy: store.RouteStrategyWeighted, Targets: targets}
}

func TestRouteCreateListGet(t *testing.T) {
	ctx := context.Background()
	f := newRouteFixture(t)

	r, err := f.s.CreateRoute(ctx, f.route("smart",
		store.RouteTarget{ConnectionID: f.b.ID, Model: strPtr("big"), Weight: 1, Priority: 1},
		store.RouteTarget{ConnectionID: f.a.ID, Weight: 3, Priority: 0}))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if r.ID == 0 || r.Name != "smart" || r.Kind != "chat" || r.Strategy != "weighted" {
		t.Fatalf("unexpected route: %+v", r)
	}
	// Targets come back ordered by priority, then id.
	if len(r.Targets) != 2 || r.Targets[0].ConnectionID != f.a.ID || r.Targets[1].ConnectionID != f.b.ID {
		t.Fatalf("targets not ordered by priority: %+v", r.Targets)
	}
	if r.Targets[0].Model != nil || r.Targets[1].Model == nil || *r.Targets[1].Model != "big" {
		t.Errorf("model override not stored: %+v", r.Targets)
	}
	if r.Targets[0].ConnectionName != "conn-a" || r.Targets[1].ProviderType != "anthropic" {
		t.Errorf("connection display fields not filled: %+v", r.Targets)
	}

	if _, err := f.s.CreateRoute(ctx, f.route("alpha", store.RouteTarget{ConnectionID: f.a.ID, Weight: 1})); err != nil {
		t.Fatal(err)
	}
	list, err := f.s.ListRoutes(ctx, f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Name != "alpha" || list[1].Name != "smart" || len(list[1].Targets) != 2 {
		t.Fatalf("list = %+v", list)
	}
	got, err := f.s.GetRoute(ctx, f.project.ID, r.ID)
	if err != nil || got.Name != "smart" {
		t.Fatalf("get: %v %+v", err, got)
	}
	if _, err := f.s.GetRoute(ctx, f.project.ID+1, r.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a route read through another project = %v, want ErrNotFound", err)
	}
}

func TestRouteNameIsUniquePerProject(t *testing.T) {
	ctx := context.Background()
	f := newRouteFixture(t)
	if _, err := f.s.CreateRoute(ctx, f.route("dup", store.RouteTarget{ConnectionID: f.a.ID, Weight: 1})); err != nil {
		t.Fatal(err)
	}
	_, err := f.s.CreateRoute(ctx, f.route("dup", store.RouteTarget{ConnectionID: f.b.ID, Weight: 1}))
	if !store.IsUniqueViolation(err) {
		t.Fatalf("duplicate name = %v, want a unique violation", err)
	}
	// Case matters: the names are never normalised (ADR-017).
	if _, err := f.s.CreateRoute(ctx, f.route("Dup", store.RouteTarget{ConnectionID: f.a.ID, Weight: 1})); err != nil {
		t.Fatalf("differently cased name: %v", err)
	}
	// A failed create leaves no half-written route behind.
	list, _ := f.s.ListRoutes(ctx, f.project.ID)
	if len(list) != 2 {
		t.Errorf("routes = %d, want 2", len(list))
	}
}

func TestRouteTargetUniquenessFoldsNullModel(t *testing.T) {
	ctx := context.Background()
	f := newRouteFixture(t)
	_, err := f.s.CreateRoute(ctx, f.route("same",
		store.RouteTarget{ConnectionID: f.a.ID, Weight: 1},
		store.RouteTarget{ConnectionID: f.a.ID, Weight: 2}))
	if !store.IsUniqueViolation(err) {
		t.Fatalf("two default-model targets on one connection = %v, want a unique violation", err)
	}
	// The same connection with distinct models is a legitimate split.
	if _, err := f.s.CreateRoute(ctx, f.route("split",
		store.RouteTarget{ConnectionID: f.a.ID, Weight: 1},
		store.RouteTarget{ConnectionID: f.a.ID, Model: strPtr("other"), Weight: 1})); err != nil {
		t.Fatalf("same connection, distinct models: %v", err)
	}
}

func TestRouteConstraintsRejectBadValues(t *testing.T) {
	ctx := context.Background()
	f := newRouteFixture(t)
	for name, r := range map[string]*store.Route{
		"weight zero":     f.route("w0", store.RouteTarget{ConnectionID: f.a.ID, Weight: 0}),
		"weight too high": f.route("w1001", store.RouteTarget{ConnectionID: f.a.ID, Weight: 1001}),
		"priority high":   f.route("p101", store.RouteTarget{ConnectionID: f.a.ID, Weight: 1, Priority: 101}),
		"priority low":    f.route("pneg", store.RouteTarget{ConnectionID: f.a.ID, Weight: 1, Priority: -1}),
		"empty name":      f.route("", store.RouteTarget{ConnectionID: f.a.ID, Weight: 1}),
		"long name":       f.route(strings.Repeat("x", store.MaxRouteNameLen+1), store.RouteTarget{ConnectionID: f.a.ID, Weight: 1}),
		"unknown conn":    f.route("ghost", store.RouteTarget{ConnectionID: f.b.ID + 1000, Weight: 1}),
	} {
		if _, err := f.s.CreateRoute(ctx, r); err == nil {
			t.Errorf("%s: create succeeded, want a constraint failure", name)
		}
	}
	bad := f.route("kind", store.RouteTarget{ConnectionID: f.a.ID, Weight: 1})
	bad.Kind = "images"
	if _, err := f.s.CreateRoute(ctx, bad); err == nil {
		t.Error("unknown kind accepted")
	}
	bad = f.route("strategy", store.RouteTarget{ConnectionID: f.a.ID, Weight: 1})
	bad.Strategy = "round_robin"
	if _, err := f.s.CreateRoute(ctx, bad); err == nil {
		t.Error("unknown strategy accepted")
	}
	list, _ := f.s.ListRoutes(ctx, f.project.ID)
	if len(list) != 0 {
		t.Errorf("rejected creates left %d routes behind", len(list))
	}
}

func TestRouteUpdateReplacesTargetsAtomically(t *testing.T) {
	ctx := context.Background()
	f := newRouteFixture(t)
	r, err := f.s.CreateRoute(ctx, f.route("r", store.RouteTarget{ConnectionID: f.a.ID, Weight: 1}))
	if err != nil {
		t.Fatal(err)
	}

	r.Name = "renamed"
	r.Targets = []store.RouteTarget{{ConnectionID: f.b.ID, Weight: 5, Priority: 2}}
	up, err := f.s.UpdateRoute(ctx, r)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if up.Name != "renamed" || len(up.Targets) != 1 || up.Targets[0].ConnectionID != f.b.ID || up.Targets[0].Weight != 5 {
		t.Fatalf("update result = %+v", up)
	}

	// A failing replacement (second target violates a CHECK) rolls the whole
	// update back: the previous target set and the name survive.
	r.Name = "again"
	r.Targets = []store.RouteTarget{{ConnectionID: f.a.ID, Weight: 1}, {ConnectionID: f.b.ID, Weight: 0}}
	if _, err := f.s.UpdateRoute(ctx, r); err == nil {
		t.Fatal("update with an invalid target succeeded")
	}
	got, err := f.s.GetRoute(ctx, f.project.ID, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "renamed" || len(got.Targets) != 1 || got.Targets[0].ConnectionID != f.b.ID {
		t.Errorf("failed update changed the route: %+v", got)
	}

	other := *r
	other.ProjectID = f.project.ID + 1
	if _, err := f.s.UpdateRoute(ctx, &other); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("update through another project = %v, want ErrNotFound", err)
	}
}

func TestRouteDeleteAndCascade(t *testing.T) {
	ctx := context.Background()
	f := newRouteFixture(t)
	r, err := f.s.CreateRoute(ctx, f.route("r", store.RouteTarget{ConnectionID: f.a.ID, Weight: 1}))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.DeleteRoute(ctx, f.project.ID+1, r.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("delete through another project = %v, want ErrNotFound", err)
	}
	if err := f.s.DeleteRoute(ctx, f.project.ID, r.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.s.DB().QueryRow(ctx, "SELECT count(*) FROM route_targets").Scan(&n); err != nil || n != 0 {
		t.Errorf("targets after route delete = %d (%v), want 0", n, err)
	}

	// Deleting a project removes its routes and their targets.
	if _, err := f.s.CreateRoute(ctx, f.route("again", store.RouteTarget{ConnectionID: f.a.ID, Weight: 1})); err != nil {
		t.Fatal(err)
	}
	if err := f.s.DeleteProject(ctx, f.project.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.s.DB().QueryRow(ctx, "SELECT (SELECT count(*) FROM project_routes) + (SELECT count(*) FROM route_targets)").Scan(&n); err != nil || n != 0 {
		t.Errorf("rows left after project delete = %d (%v), want 0", n, err)
	}
}

func TestConnectionInRouteCannotBeDeleted(t *testing.T) {
	ctx := context.Background()
	f := newRouteFixture(t)
	r, err := f.s.CreateRoute(ctx, f.route("r", store.RouteTarget{ConnectionID: f.b.ID, Weight: 1}))
	if err != nil {
		t.Fatal(err)
	}
	// conn-b is used by no project, only by the route target.
	if err := f.s.DeleteConnection(ctx, f.b.ID); !store.IsForeignKeyViolation(err) {
		t.Fatalf("delete of a routed connection = %v, want a foreign key violation", err)
	}
	if err := f.s.DeleteRoute(ctx, f.project.ID, r.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.s.DeleteConnection(ctx, f.b.ID); err != nil {
		t.Fatalf("delete after the route is gone: %v", err)
	}
}

func TestResolveProjectRoute(t *testing.T) {
	ctx := context.Background()
	f := newRouteFixture(t)

	// No route at all: the project does not route.
	has, rt, err := f.s.ResolveProjectRoute(ctx, f.project.ID, store.RouteKindChat, "chat1")
	if err != nil || has || rt != nil {
		t.Fatalf("empty project: has=%v route=%v err=%v", has, rt, err)
	}

	if _, err := f.s.CreateRoute(ctx, f.route("chat1",
		store.RouteTarget{ConnectionID: f.a.ID, Weight: 1},
		store.RouteTarget{ConnectionID: f.b.ID, Weight: 2, Priority: 1})); err != nil {
		t.Fatal(err)
	}
	emb := f.route("emb1", store.RouteTarget{ConnectionID: f.a.ID, Weight: 1})
	emb.Kind = store.RouteKindEmbeddings
	if _, err := f.s.CreateRoute(ctx, emb); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.CreateRoute(ctx, f.route("chat2", store.RouteTarget{ConnectionID: f.b.ID, Weight: 1})); err != nil {
		t.Fatal(err)
	}

	has, rt, err = f.s.ResolveProjectRoute(ctx, f.project.ID, store.RouteKindChat, "chat1")
	if err != nil || !has || rt == nil {
		t.Fatalf("chat1: has=%v route=%v err=%v", has, rt, err)
	}
	if rt.Name != "chat1" || rt.Kind != "chat" || rt.Strategy != "weighted" || rt.ID == 0 || len(rt.Targets) != 2 {
		t.Fatalf("route = %+v", rt)
	}
	t0, t1 := rt.Targets[0], rt.Targets[1]
	if t0.Connection == nil || t0.Connection.APIKey != "sk-secret-a" || t0.Connection.ModelName != "model-a" {
		t.Errorf("target 0 connection = %+v", t0.Connection)
	}
	if t1.Connection == nil || t1.Connection.APIKey != "sk-secret-b" || t1.Weight != 2 || t1.Priority != 1 {
		t.Errorf("target 1 = %+v", t1)
	}

	// The project routes, but nothing is named like this: has is true and the
	// route is nil. A name that is another kind's, unknown, empty or holds a NUL
	// byte (PostgreSQL text rejects it) all end here, never in an error.
	for _, name := range []string{"emb1", "nope", "", "chat1\x00", "\x00", "chat1 "} {
		has, rt, err := f.s.ResolveProjectRoute(ctx, f.project.ID, store.RouteKindChat, name)
		if err != nil || !has || rt != nil {
			t.Errorf("name %q: has=%v route=%v err=%v, want has and no route", name, has, rt, err)
		}
	}

	// Only the project's own routes count.
	has, rt, err = f.s.ResolveProjectRoute(ctx, f.project.ID+1, store.RouteKindChat, "chat1")
	if err != nil || has || rt != nil {
		t.Errorf("another project: has=%v route=%v err=%v", has, rt, err)
	}
	// A kind with no route of its own does not route.
	has, rt, err = f.s.ResolveProjectRoute(ctx, f.project.ID, store.RouteKindRerank, "chat1")
	if err != nil || has || rt != nil {
		t.Errorf("rerank kind: has=%v route=%v err=%v", has, rt, err)
	}
}

// A connection whose key cannot be decrypted, used by another route, must not
// fail the lookup of the route that was asked for: only the named route's
// connections are decrypted.
func TestResolveProjectRouteIgnoresOtherRoutesBrokenTarget(t *testing.T) {
	ctx := context.Background()
	f := newRouteFixture(t)
	if _, err := f.s.CreateRoute(ctx, f.route("good", store.RouteTarget{ConnectionID: f.a.ID, Weight: 1})); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.CreateRoute(ctx, f.route("broken", store.RouteTarget{ConnectionID: f.b.ID, Weight: 1})); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.DB().Exec(ctx, "UPDATE model_connections SET api_key_enc = '\\x0102030405'::bytea WHERE id = $1", f.b.ID); err != nil {
		t.Fatal(err)
	}

	has, rt, err := f.s.ResolveProjectRoute(ctx, f.project.ID, store.RouteKindChat, "good")
	if err != nil || !has || rt == nil || len(rt.Targets) != 1 || rt.Targets[0].Connection.APIKey != "sk-secret-a" {
		t.Fatalf("good route: has=%v route=%+v err=%v", has, rt, err)
	}
	if _, _, err := f.s.ResolveProjectRoute(ctx, f.project.ID, store.RouteKindChat, "broken"); err == nil {
		t.Error("the broken route itself must fail to resolve")
	}
	has, rt, err = f.s.ResolveProjectRoute(ctx, f.project.ID, store.RouteKindChat, "unknown")
	if err != nil || !has || rt != nil {
		t.Errorf("unknown name next to a broken route: has=%v route=%v err=%v", has, rt, err)
	}
}

func TestGetRouteHugeIDIsNotFound(t *testing.T) {
	f := newRouteFixture(t)
	// An id past int32 must be a clean miss, not an encoding error.
	if _, err := f.s.GetRoute(context.Background(), f.project.ID, 9999999999); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestConnectionRouteKinds(t *testing.T) {
	ctx := context.Background()
	f := newRouteFixture(t)
	kinds, err := f.s.ConnectionRouteKinds(ctx, f.a.ID)
	if err != nil || len(kinds) != 0 {
		t.Fatalf("unused connection: %v %v", kinds, err)
	}
	if _, err := f.s.CreateRoute(ctx, f.route("c1", store.RouteTarget{ConnectionID: f.a.ID, Weight: 1})); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.CreateRoute(ctx, f.route("c2", store.RouteTarget{ConnectionID: f.a.ID, Weight: 1})); err != nil {
		t.Fatal(err)
	}
	emb := f.route("e1", store.RouteTarget{ConnectionID: f.a.ID, Weight: 1})
	emb.Kind = store.RouteKindEmbeddings
	if _, err := f.s.CreateRoute(ctx, emb); err != nil {
		t.Fatal(err)
	}
	kinds, err = f.s.ConnectionRouteKinds(ctx, f.a.ID)
	if err != nil || len(kinds) != 2 || kinds[0] != "chat" || kinds[1] != "embeddings" {
		t.Fatalf("kinds = %v (%v), want [chat embeddings]", kinds, err)
	}
	if kinds, _ := f.s.ConnectionRouteKinds(ctx, f.b.ID); len(kinds) != 0 {
		t.Errorf("other connection kinds = %v", kinds)
	}
}
