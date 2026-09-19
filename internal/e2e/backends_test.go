package e2e

import (
	"fmt"
	"strings"
	"testing"

	"github.com/ragmux/ragmux/internal/testdb"
)

// These run on the ordinary pgvector CI job, which is exactly the point: the
// write-time rejection and the read-time fallback only mean anything on a
// server that does *not* carry pg_search, and that job is what proves it.
func TestSearchBackendsEndpointAndValidation(t *testing.T) {
	e := newEnv(t, testdb.Config(t))
	status := func(r map[string]any) int { return int(r["_status"].(float64)) }

	list := e.call("GET", "/admin/api/search-backends", nil, "")["_list"].([]any)
	if len(list) != 2 {
		t.Fatalf("search backends: %v", list)
	}
	byID := map[string]map[string]any{}
	for _, it := range list {
		m := it.(map[string]any)
		byID[m["id"].(string)] = m
	}
	if byID["pgvector"]["available"] != true || byID["pgvector"]["reason"] != "" {
		t.Errorf("pgvector must always be available: %v", byID["pgvector"])
	}
	pgs := byID["pg_search"]
	if pgs == nil {
		t.Fatalf("pg_search missing from the listing: %v", list)
	}
	if e.store.Caps().PgSearch {
		t.Skip("this server carries pg_search; the rejection path needs one without it")
	}
	if pgs["available"] != false || !strings.Contains(pgs["reason"].(string), "pg_search") {
		t.Errorf("pg_search should be unavailable with a reason: %v", pgs)
	}

	conn := e.call("POST", "/admin/api/models", map[string]any{"name": "emb", "provider_type": "openai",
		"model_name": "text-embedding-3-small", "api_key": "sk-x"}, "")
	connID := conn["id"].(float64)

	base := func(extra map[string]any) map[string]any {
		body := map[string]any{"name": "docs", "embedding_connection_id": connID,
			"chunk_size": 500, "chunk_overlap": 50, "top_k": 3}
		for k, v := range extra {
			body[k] = v
		}
		return body
	}

	// Write time: rejected with 400, naming the extension and the docs.
	bad := e.call("POST", "/admin/api/rag-stores", base(map[string]any{"search_backend": "pg_search"}), "")
	if status(bad) != 400 {
		t.Fatalf("pg_search should be rejected here: %v", bad)
	}
	msg := fmt.Sprint(bad["error"])
	if !strings.Contains(msg, "pg_search") || !strings.Contains(msg, "docs/rag.md#search-backends") {
		t.Errorf("rejection message should name the extension and the docs: %q", msg)
	}
	if r := e.call("POST", "/admin/api/rag-stores", base(map[string]any{"search_backend": "lucene"}), ""); status(r) != 400 {
		t.Errorf("unknown backend should be rejected: %v", r)
	}

	// The default is pgvector and it saves.
	ok := e.call("POST", "/admin/api/rag-stores", base(nil), "")
	if status(ok) != 201 || ok["search_backend"] != "pgvector" || ok["rerank_backend"] != "llm" || ok["rerank_connection_id"] != nil {
		t.Fatalf("default store: %v", ok)
	}
	id := ok["id"].(float64)

	// A backend change is not a reprocess: both backends read chunks.content.
	upd := e.call("PUT", fmt.Sprintf("/admin/api/rag-stores/%d", int64(id)),
		base(map[string]any{"search_backend": "pgvector", "fts_config": "english"}), "")
	if status(upd) != 200 || upd["reprocess_recommended"] != false {
		t.Errorf("backend/config change must not recommend a reprocess: %v", upd)
	}

	// The search response names the effective backend and echoes fts_config.
	sr := e.call("POST", fmt.Sprintf("/admin/api/rag-stores/%d/search", int64(id)), map[string]any{"query": "anything"}, "")
	if status(sr) != 200 || sr["backend"] != "pgvector" || sr["fts_config"] != "english" {
		t.Errorf("search response: %v", sr)
	}
	if r := e.call("POST", fmt.Sprintf("/admin/api/rag-stores/%d/search", int64(id)),
		map[string]any{"query": "q", "backend": "nope"}, ""); status(r) != 400 {
		t.Errorf("unknown backend override should be rejected: %v", r)
	}
}

func TestRerankBackendValidation(t *testing.T) {
	e := newEnv(t, testdb.Config(t))
	status := func(r map[string]any) int { return int(r["_status"].(float64)) }

	emb := e.call("POST", "/admin/api/models", map[string]any{"name": "emb", "provider_type": "openai",
		"model_name": "text-embedding-3-small", "api_key": "sk-x"}, "")
	chat := e.call("POST", "/admin/api/models", map[string]any{"name": "chat", "provider_type": "openai",
		"model_name": "gpt-4o", "api_key": "sk-x"}, "")
	co := e.call("POST", "/admin/api/models", map[string]any{"name": "cohere", "provider_type": "cohere_rerank",
		"model_name": "rerank-v3.5", "api_key": "sk-co"}, "")
	if status(co) != 201 {
		t.Fatalf("cohere_rerank connection: %v", co)
	}
	// It is a real connection with a masked, encrypted credential like any
	// other, which is the whole reason the credentials live here.
	if co["api_key_masked"] == "" || co["api_key_masked"] == "sk-co" {
		t.Errorf("rerank credential should be masked: %v", co["api_key_masked"])
	}

	base := func(extra map[string]any) map[string]any {
		body := map[string]any{"name": "docs", "embedding_connection_id": emb["id"],
			"chunk_size": 500, "chunk_overlap": 50, "top_k": 3, "rerank": true}
		for k, v := range extra {
			body[k] = v
		}
		return body
	}

	if r := e.call("POST", "/admin/api/rag-stores", base(map[string]any{"rerank_backend": "nope"}), ""); status(r) != 400 {
		t.Errorf("unknown rerank backend: %v", r)
	}
	// An API backend must name a connection.
	r := e.call("POST", "/admin/api/rag-stores", base(map[string]any{"rerank_backend": "cohere"}), "")
	if status(r) != 400 || !strings.Contains(fmt.Sprint(r["error"]), "rerank_connection_id") {
		t.Errorf("cohere without a connection: %v", r)
	}
	// ... and it must be the right kind of connection.
	r = e.call("POST", "/admin/api/rag-stores", base(map[string]any{"rerank_backend": "cohere", "rerank_connection_id": chat["id"]}), "")
	if status(r) != 400 || !strings.Contains(fmt.Sprint(r["error"]), `cannot use connection "chat" for reranking: it is a openai connection`) {
		t.Errorf("cohere with a chat connection: %v", r)
	}
	r = e.call("POST", "/admin/api/rag-stores", base(map[string]any{"rerank_backend": "voyage", "rerank_connection_id": co["id"]}), "")
	if status(r) != 400 || !strings.Contains(fmt.Sprint(r["error"]), "cohere_rerank connection") {
		t.Errorf("voyage with a cohere connection: %v", r)
	}

	created := e.call("POST", "/admin/api/rag-stores", base(map[string]any{"rerank_backend": "cohere", "rerank_connection_id": co["id"]}), "")
	if status(created) != 201 || created["rerank_backend"] != "cohere" || created["rerank_connection_id"] != co["id"] {
		t.Fatalf("cohere store: %v", created)
	}
	// Switching back to the LLM backend drops the connection rather than
	// leaving it to reappear on a later switch.
	back := e.call("PUT", fmt.Sprintf("/admin/api/rag-stores/%d", int64(created["id"].(float64))),
		base(map[string]any{"rerank_backend": "llm", "rerank_connection_id": co["id"]}), "")
	if status(back) != 200 || back["rerank_backend"] != "llm" || back["rerank_connection_id"] != nil {
		t.Errorf("switch back to llm: %v", back)
	}

	// A rerank-only connection can back neither a project nor a RAG store.
	proj := e.call("POST", "/admin/api/projects", map[string]any{"name": "p", "model_connection_id": co["id"]}, "")
	if status(proj) != 400 || !strings.Contains(fmt.Sprint(proj["error"]), "cannot be used for chat") {
		t.Errorf("project on a rerank connection: %v", proj)
	}
	embStore := e.call("POST", "/admin/api/rag-stores", map[string]any{"name": "x", "embedding_connection_id": co["id"],
		"chunk_size": 500, "chunk_overlap": 50, "top_k": 3}, "")
	if status(embStore) != 400 || !strings.Contains(fmt.Sprint(embStore["error"]), "cannot be used for embeddings") {
		t.Errorf("rag store on a rerank connection: %v", embStore)
	}
}
