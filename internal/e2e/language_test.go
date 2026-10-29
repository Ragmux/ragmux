package e2e

import (
	"context"
	"fmt"
	"testing"

	"github.com/ragmux/ragmux/internal/testdb"
)

// The store language over the admin API (ADR-027): language is the setting,
// fts_config its deprecated, derived echo.
func TestStoreLanguageOverAdminAPI(t *testing.T) {
	e := newEnv(t, testdb.Config(t))
	status := func(r map[string]any) int { return int(r["_status"].(float64)) }
	msg := func(r map[string]any) string { return r["error"].(map[string]any)["message"].(string) }

	conn := e.call("POST", "/admin/api/models", map[string]any{"name": "emb", "provider_type": "openai",
		"model_name": "text-embedding-3-small", "api_key": "sk-x"}, "")
	connID := conn["id"].(float64)
	body := func(extra map[string]any) map[string]any {
		b := map[string]any{"name": "docs", "embedding_connection_id": connID, "chunk_size": 500, "chunk_overlap": 50, "top_k": 3}
		for k, v := range extra {
			b[k] = v
		}
		return b
	}

	// An old client that only knows fts_config gets the language it names.
	old := e.call("POST", "/admin/api/rag-stores", body(map[string]any{"fts_config": "english"}), "")
	if status(old) != 201 || old["language"] != "en" || old["fts_config"] != "english" || old["fts_status"] != "ready" {
		t.Fatalf("fts_config-only create: %v", old)
	}

	// A contradiction, an unmapped code and an unknown configuration are 400s.
	for name, extra := range map[string]map[string]any{
		"contradiction":   {"language": "tr", "fts_config": "english"},
		"unmapped code":   {"language": "xx"},
		"language name":   {"language": "turkish"},
		"unknown config":  {"fts_config": "klingon"},
		"simple vs. lang": {"language": "tr", "fts_config": "simple"},
	} {
		if r := e.call("POST", "/admin/api/rag-stores", body(extra), ""); status(r) != 400 {
			t.Errorf("%s: want 400, got %v", name, r)
		} else if m := msg(r); m == "" {
			t.Errorf("%s: empty message", name)
		}
	}
	// The same on update.
	oldID := int64(old["id"].(float64))
	url := fmt.Sprintf("/admin/api/rag-stores/%d", oldID)
	if r := e.call("PUT", url, body(map[string]any{"language": "tr", "fts_config": "english"}), ""); status(r) != 400 {
		t.Errorf("update with a contradiction: want 400, got %v", r)
	}
	if got := e.call("GET", url, nil, ""); got["language"] != "en" {
		t.Errorf("a rejected update must change nothing: %v", got)
	}

	// language is the setting; a store without chunks is ready at once.
	tr := e.call("POST", "/admin/api/rag-stores", body(map[string]any{"name": "tr", "language": "TR"}), "")
	if status(tr) != 201 || tr["language"] != "tr" || tr["fts_config"] != "turkish" || tr["fts_status"] != "ready" ||
		tr["language_applies_to_search"] != true || tr["fts_language_applied"] != "tr" {
		t.Fatalf("language create: %v", tr)
	}
	trURL := fmt.Sprintf("/admin/api/rag-stores/%d", int64(tr["id"].(float64)))

	// An update that names neither keeps the language; one that names only
	// fts_config follows it.
	if r := e.call("PUT", trURL, body(map[string]any{"name": "tr", "top_k": 4}), ""); status(r) != 200 || r["language"] != "tr" {
		t.Errorf("update without a language: %v", r)
	}
	if r := e.call("PUT", trURL, body(map[string]any{"name": "tr", "fts_config": "german"}), ""); status(r) != 200 || r["language"] != "de" || r["fts_config"] != "german" {
		t.Errorf("update with fts_config only: %v", r)
	}
	if r := e.call("PUT", trURL, body(map[string]any{"name": "tr", "language": "simple"}), ""); status(r) != 200 || r["language"] != "simple" || r["fts_status"] != "not_needed" {
		t.Errorf("update to simple: %v", r)
	}

	// A store saved before 0027 keeps its leftover fts_config through a save
	// that only echoes the derived one (the dashboard), and sheds it when the
	// operator names the language.
	rawURL := trURL
	setRaw := func() {
		if _, err := e.store.DB().Exec(context.Background(), "UPDATE rag_stores SET fts_config = 'turkish' WHERE id = $1",
			int64(tr["id"].(float64))); err != nil {
			t.Fatal(err)
		}
	}
	raw := func() (cfg string) {
		if err := e.store.DB().QueryRow(context.Background(), "SELECT fts_config FROM rag_stores WHERE id = $1",
			int64(tr["id"].(float64))).Scan(&cfg); err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	setRaw()
	if r := e.call("PUT", rawURL, body(map[string]any{"name": "tr", "fts_config": "simple"}), ""); status(r) != 200 || raw() != "turkish" {
		t.Errorf("fts_config echo must keep the leftover: %v raw=%q", r, raw())
	}
	if r := e.call("PUT", rawURL, body(map[string]any{"name": "tr", "language": "simple"}), ""); status(r) != 200 || raw() != "simple" {
		t.Errorf("an explicit language must clear the leftover: %v raw=%q", r, raw())
	}

	// The search response says which language took part.
	sr := e.call("POST", trURL+"/search", map[string]any{"query": "anything"}, "")
	if status(sr) != 200 || sr["language"] != "simple" || sr["fts_config"] != "simple" || sr["language_applied_to_search"] != true {
		t.Errorf("search response: %v", sr)
	}
	// A vector search never runs the lexical leg, so no language took part.
	vr := e.call("POST", trURL+"/search", map[string]any{"query": "anything", "mode": "vector"}, "")
	if status(vr) != 200 || vr["mode"] != "vector" || vr["language_applied_to_search"] != false {
		t.Errorf("vector search response: %v", vr)
	}
}
