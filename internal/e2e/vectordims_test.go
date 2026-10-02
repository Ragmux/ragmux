package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ragmux/ragmux/internal/testdb"
)

// wideEmbeddingUpstream answers every embedding request with vectors of the
// given width, standing in for a model like text-embedding-3-large.
func wideEmbeddingUpstream(dims int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.Unmarshal(body, &req)
		data := make([]map[string]any, len(req.Input))
		for i := range req.Input {
			vec := make([]float32, dims)
			vec[i%dims] = 1
			data[i] = map[string]any{"index": i, "embedding": vec}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
}

// A store bound to an embedding model wider than pgvector can index is refused
// with a 400 that names the limit and the halfvec plan, both on create and on
// an edit that switches to that model.
func TestRAGStoreRefusesUnindexableEmbeddingWidth(t *testing.T) {
	wide := wideEmbeddingUpstream(3072)
	defer wide.Close()
	narrow := mockUpstream(t)
	defer narrow.Close()

	e := newEnv(t, testdb.Config(t))
	status := func(r map[string]any) int { return int(r["_status"].(float64)) }
	newConn := func(name, baseURL string) float64 {
		t.Helper()
		c := e.call("POST", "/admin/api/models", map[string]any{"name": name, "provider_type": "custom_openai",
			"base_url": baseURL + "/v1", "api_key": "secret", "model_name": name}, "")
		if status(c) != 201 {
			t.Fatalf("create connection %s: %v", name, c)
		}
		return c["id"].(float64)
	}
	wideID := newConn("wide", wide.URL)
	narrowID := newConn("narrow", narrow.URL)
	body := func(connID float64) map[string]any {
		return map[string]any{"name": "docs", "embedding_connection_id": connID,
			"chunk_size": 500, "chunk_overlap": 50, "top_k": 3}
	}
	wantRefusal := func(what string, r map[string]any) {
		t.Helper()
		if status(r) != 400 {
			t.Fatalf("%s: want 400, got %v", what, r)
		}
		msg := fmt.Sprint(r["error"])
		for _, part := range []string{"3072", "2000", "halfvec", "v0.6"} {
			if !strings.Contains(msg, part) {
				t.Errorf("%s: message should mention %q: %q", what, part, msg)
			}
		}
	}

	wantRefusal("create", e.call("POST", "/admin/api/rag-stores", body(wideID), ""))

	ok := e.call("POST", "/admin/api/rag-stores", body(narrowID), "")
	if status(ok) != 201 {
		t.Fatalf("a narrow embedding model should save: %v", ok)
	}
	path := fmt.Sprintf("/admin/api/rag-stores/%d", int64(ok["id"].(float64)))
	wantRefusal("edit", e.call("PUT", path, body(wideID), ""))

	got := e.call("GET", path, nil, "")
	if got["embedding_connection_id"] != narrowID {
		t.Errorf("a refused edit must leave the store bound to its model: %v", got)
	}
}
