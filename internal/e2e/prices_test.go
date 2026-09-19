package e2e

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"

	"github.com/ragmux/ragmux/internal/pricing"
	"github.com/ragmux/ragmux/internal/testdb"
)

// findPriceRow returns one row of GET /prices by provider type and pattern.
func findPriceRow(t *testing.T, rows []any, providerType, pattern string) map[string]any {
	t.Helper()
	for _, raw := range rows {
		p := raw.(map[string]any)
		if p["provider_type"] == providerType && p["model_pattern"] == pattern {
			return p
		}
	}
	t.Fatalf("no price row for %s/%s", providerType, pattern)
	return nil
}

// TestModelPricesCRUD walks the price API the way the dashboard does: the
// role matrix, a built-in row that cannot be deleted, an edit that turns it
// into the operator's own, a reset that gives it back, and the audit trail.
func TestModelPricesCRUD(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, testdb.Config(t))
	status := func(r map[string]any) int { return int(r["_status"].(float64)) }
	errOf := func(r map[string]any) map[string]any { m, _ := r["error"].(map[string]any); return m }
	// main.go seeds on start; the e2e harness wires the server directly.
	if _, err := pricing.Seed(ctx, e.store.DB(), slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	e.call("POST", "/admin/api/users", map[string]any{"username": "ed", "password": "editorpass", "role": "editor"}, "")
	e.call("POST", "/admin/api/users", map[string]any{"username": "vw", "password": "viewerpass", "role": "viewer"}, "")
	editor, viewer := e.login("ed", "editorpass"), e.login("vw", "viewerpass")

	list := e.call("GET", "/admin/api/prices", nil, viewer)
	if status(list) != 200 {
		t.Fatalf("viewer cannot read prices: %v", list)
	}
	rows := list["prices"].([]any)
	if len(rows) == 0 || list["builtin_version"] == nil || list["unit"] != "per_million_tokens" {
		t.Fatalf("price listing: %v", list)
	}
	builtin := findPriceRow(t, rows, "anthropic", "claude-sonnet-4-5*")
	builtinID := int64(builtin["id"].(float64))
	if builtin["source"] != "builtin" {
		t.Errorf("seeded row source = %v", builtin["source"])
	}

	// Role matrix: every mutation needs the editor role.
	body := map[string]any{"provider_type": "custom_openai", "model_pattern": "my-model*",
		"input_per_mtok": 1.5, "output_per_mtok": 3, "currency": "usd"}
	for _, c := range []struct {
		method, path string
		payload      any
	}{
		{"POST", "/admin/api/prices", body},
		{"PUT", fmt.Sprintf("/admin/api/prices/%d", builtinID), body},
		{"DELETE", fmt.Sprintf("/admin/api/prices/%d", builtinID), nil},
		{"POST", fmt.Sprintf("/admin/api/prices/%d/reset", builtinID), map[string]any{}},
	} {
		if r := e.call(c.method, c.path, c.payload, viewer); status(r) != 403 {
			t.Errorf("viewer %s %s = %v, want 403", c.method, c.path, status(r))
		}
	}

	// A built-in row is never deleted, only edited or reset.
	del := e.call("DELETE", fmt.Sprintf("/admin/api/prices/%d", builtinID), nil, editor)
	if status(del) != 409 || errOf(del)["code"] != "builtin_price" {
		t.Errorf("deleting a built-in price: %v", del)
	}

	// Editing turns it into the operator's own row.
	upd := e.call("PUT", fmt.Sprintf("/admin/api/prices/%d", builtinID),
		map[string]any{"input_per_mtok": 9, "output_per_mtok": 18, "cache_read_per_mtok": 0.9, "currency": "usd"}, editor)
	if status(upd) != 200 || upd["source"] != "user" || upd["input_per_mtok"] != float64(9) || upd["currency"] != "USD" {
		t.Fatalf("update: %v", upd)
	}
	// And a re-seed leaves it alone.
	if n, err := pricing.Seed(ctx, e.store.DB(), slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil || n != 0 {
		t.Errorf("re-seed touched %d rows: %v", n, err)
	}

	// Reset restores the shipped numbers and the built-in source.
	reset := e.call("POST", fmt.Sprintf("/admin/api/prices/%d/reset", builtinID), map[string]any{}, editor)
	if status(reset) != 200 || reset["source"] != "builtin" || reset["input_per_mtok"] != builtin["input_per_mtok"] {
		t.Fatalf("reset: %v (was %v)", reset, builtin["input_per_mtok"])
	}

	// An operator's own row: created, rejected as a duplicate, deleted.
	created := e.call("POST", "/admin/api/prices", body, editor)
	if status(created) != 201 || created["source"] != "user" || created["currency"] != "USD" {
		t.Fatalf("create: %v", created)
	}
	ownID := int64(created["id"].(float64))
	if r := e.call("POST", "/admin/api/prices", body, editor); status(r) != 409 {
		t.Errorf("duplicate pattern: %v", r)
	}
	// A row with no built-in counterpart cannot be reset.
	if r := e.call("POST", fmt.Sprintf("/admin/api/prices/%d/reset", ownID), map[string]any{}, editor); status(r) != 409 || errOf(r)["code"] != "no_builtin_price" {
		t.Errorf("reset of an own row: %v", r)
	}
	// Validation.
	for _, bad := range []map[string]any{
		{"provider_type": "nope", "model_pattern": "x", "input_per_mtok": 1},
		{"provider_type": "openai", "model_pattern": "", "input_per_mtok": 1},
		{"provider_type": "openai", "model_pattern": "x", "input_per_mtok": -1},
		{"provider_type": "openai", "model_pattern": "x", "input_per_mtok": 1e9},
		{"provider_type": "openai", "model_pattern": "x", "currency": "dollars"},
	} {
		if r := e.call("POST", "/admin/api/prices", bad, editor); status(r) != 400 {
			t.Errorf("invalid body %v accepted: %v", bad, r)
		}
	}
	if r := e.call("DELETE", fmt.Sprintf("/admin/api/prices/%d", ownID), nil, editor); status(r) != 200 {
		t.Errorf("delete own row: %v", r)
	}
	if r := e.call("GET", "/admin/api/prices", nil, editor); len(r["prices"].([]any)) != len(rows) {
		t.Errorf("table size after the round trip: %d, want %d", len(r["prices"].([]any)), len(rows))
	}

	// Every mutation is audited; the failed ones are not.
	audit := e.call("GET", "/admin/api/audit?action=price.&limit=100", nil, "")
	seen := map[string]int{}
	for _, raw := range audit["entries"].([]any) {
		a := raw.(map[string]any)
		seen[a["action"].(string)]++
		if a["actor_username"] != "ed" || a["target_type"] != "price" {
			t.Errorf("audit entry = %v", a)
		}
	}
	for action, want := range map[string]int{"price.update": 1, "price.reset": 1, "price.create": 1, "price.delete": 1} {
		if seen[action] != want {
			t.Errorf("audit %s = %d, want %d (all: %v)", action, seen[action], want, seen)
		}
	}
}
