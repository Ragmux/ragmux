package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ragmux/ragmux/internal/admin"
	"github.com/ragmux/ragmux/internal/store"
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

// A store bound to an embedding model wider than any HNSW index covers (4000)
// is refused with a 400 that names the limit and the binary quantization plan,
// both on create and on an edit that switches to that model.
func TestRAGStoreRefusesUnindexableEmbeddingWidth(t *testing.T) {
	wide := wideEmbeddingUpstream(4097)
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
		for _, part := range []string{"4097", "4000", "binary quantization", "v0.9"} {
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

// vectorEnv is an env with a connection to an embedding model of the given
// width and a helper to make stores on it.
type vectorEnv struct {
	*env
	t      *testing.T
	connID float64
}

func newVectorEnv(t *testing.T, dims int) *vectorEnv {
	t.Helper()
	up := wideEmbeddingUpstream(dims)
	t.Cleanup(up.Close)
	e := newEnv(t, testdb.Config(t))
	if !e.store.Caps().SupportsHalfvec() {
		t.Skipf("pgvector %q has no halfvec", e.store.Caps().PgvectorVersion)
	}
	c := e.call("POST", "/admin/api/models", map[string]any{"name": fmt.Sprintf("emb-%d", dims), "provider_type": "custom_openai",
		"base_url": up.URL + "/v1", "api_key": "secret", "model_name": "emb"}, "")
	if c["_status"] != float64(201) {
		t.Fatalf("create connection: %v", c)
	}
	return &vectorEnv{env: e, t: t, connID: c["id"].(float64)}
}

func (v *vectorEnv) createStore(name string) map[string]any {
	return v.call("POST", "/admin/api/rag-stores", map[string]any{"name": name, "embedding_connection_id": v.connID,
		"chunk_size": 500, "chunk_overlap": 50, "top_k": 3}, "")
}

func (v *vectorEnv) storeIndex(id float64) map[string]any {
	return v.call("GET", fmt.Sprintf("/admin/api/rag-stores/%d", int64(id)), nil, "")
}

func (v *vectorEnv) waitVectorIndex(id float64, want string) {
	v.t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for {
		got := v.storeIndex(id)["vector_index"]
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			v.t.Fatalf("vector_index is %v, want %s", got, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func errorCode(r map[string]any) string {
	e, _ := r["error"].(map[string]any)
	code, _ := e["code"].(string)
	return code
}

// A 3072-dimension model works: the store saves, ingests, searches, and says
// its vectors sit behind a halfvec index.
func TestHalfvecStoreIngestsAndSearches(t *testing.T) {
	v := newVectorEnv(t, 3072)
	created := v.createStore("wide")
	if created["_status"] != float64(201) {
		t.Fatalf("a 3072-dimension model should save: %v", created)
	}
	id := created["id"].(float64)
	// Not bound to a width until the first ingest.
	if created["vector_index"] != nil {
		t.Errorf("an unbound store reports a vector index: %v", created["vector_index"])
	}
	doc := v.upload(int64(id), "a.txt", "the quick brown fox")
	if doc["_status"] != float64(201) && doc["_status"] != float64(202) {
		t.Fatalf("upload: %v", doc)
	}
	v.waitReady(int64(doc["id"].(float64)))

	got := v.storeIndex(id)
	if got["dimensions"] != float64(3072) || got["vector_index"] != "hnsw_halfvec" || got["vector_index_progress"] != nil {
		t.Errorf("store after ingest: dimensions=%v vector_index=%v progress=%v", got["dimensions"], got["vector_index"], got["vector_index_progress"])
	}
	list := v.call("GET", "/admin/api/rag-stores", nil, "")["_list"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["vector_index"] != "hnsw_halfvec" {
		t.Errorf("store list: %v", list)
	}
	res := v.call("POST", fmt.Sprintf("/admin/api/rag-stores/%d/search", int64(id)), map[string]any{"query": "fox"}, "")
	hits, _ := res["hits"].([]any)
	if res["_status"] != float64(200) || len(hits) == 0 {
		t.Fatalf("search: %v", res)
	}
	if d := hits[0].(map[string]any)["distance"].(float64); d > 1e-6 {
		t.Errorf("distance of the identical vector = %v, want 0", d)
	}
}

// The reindex action: admin only, asynchronous, per width, and the only way
// out for a width that is bound but has no halfvec index.
func TestReindexEndpoint(t *testing.T) {
	v := newVectorEnv(t, 2500)
	a := v.createStore("legacy")
	if a["_status"] != float64(201) {
		t.Fatalf("create: %v", a)
	}
	aID := a["id"].(float64)
	doc := v.upload(int64(aID), "a.txt", "some text")
	v.waitReady(int64(doc["id"].(float64)))
	reindex := func(token string, id float64) map[string]any {
		return v.call("POST", fmt.Sprintf("/admin/api/rag-stores/%d/reindex", int64(id)), nil, token)
	}

	// Born indexed: nothing to do.
	if r := reindex("", aID); r["_status"] != float64(409) || errorCode(r) != "already_indexed" {
		t.Fatalf("reindex of an indexed width: %v", r)
	}

	// What an upgraded installation looks like: the table without its index.
	if _, err := v.store.DB().Exec(context.Background(), "DROP INDEX chunk_embeddings_2500_hnsw_half"); err != nil {
		t.Fatal(err)
	}
	if got := v.storeIndex(aID); got["vector_index"] != "missing" {
		t.Fatalf("vector_index without the index = %v, want missing", got["vector_index"])
	}
	// Search still answers, from an exact scan.
	res := v.call("POST", fmt.Sprintf("/admin/api/rag-stores/%d/search", int64(aID)), map[string]any{"query": "text"}, "")
	if hits, _ := res["hits"].([]any); res["_status"] != float64(200) || len(hits) == 0 {
		t.Fatalf("search without the index: %v", res)
	}
	// A new store on the width is turned away until it is reindexed.
	if r := v.createStore("newcomer"); r["_status"] != float64(409) || errorCode(r) != "index_missing" ||
		!strings.Contains(fmt.Sprint(r["error"]), "reindex") {
		t.Fatalf("create on an index-less width: %v", r)
	}

	// Only an admin may start it.
	if u := v.call("POST", "/admin/api/users", map[string]any{"username": "ed", "password": "editor-pass-1", "role": "editor"}, ""); u["_status"] != float64(201) {
		t.Fatalf("create editor: %v", u)
	}
	if r := reindex(v.login("ed", "editor-pass-1"), aID); r["_status"] != float64(403) {
		t.Errorf("reindex as editor: %v", r)
	}
	if r := reindex("", 99999); r["_status"] != float64(404) {
		t.Errorf("reindex of an unknown store: %v", r)
	}

	r := reindex("", aID)
	if r["_status"] != float64(202) || r["dimensions"] != float64(2500) {
		t.Fatalf("reindex: %v", r)
	}
	if st := r["vector_index"]; st != "building" {
		t.Errorf("reindex answer vector_index = %v, want building", st)
	}
	affected, _ := r["affected_stores"].([]any)
	if len(affected) != 1 || affected[0].(map[string]any)["id"] != aID || affected[0].(map[string]any)["name"] != "legacy" {
		t.Errorf("affected stores = %v", affected)
	}
	v.waitVectorIndex(aID, "hnsw_halfvec")
	if r := reindex("", aID); r["_status"] != float64(409) || errorCode(r) != "already_indexed" {
		t.Errorf("second reindex: %v", r)
	}
	if r := v.createStore("newcomer"); r["_status"] != float64(201) {
		t.Errorf("create after the reindex: %v", r)
	}
	audit := v.call("GET", "/admin/api/audit?action=rag_store.reindex", nil, "")
	if list, _ := audit["entries"].([]any); len(list) != 1 {
		t.Errorf("rag_store.reindex audit rows: %v", audit)
	}

	// A store that has not ingested anything has no width, hence no index.
	for _, it := range v.call("GET", "/admin/api/rag-stores", nil, "")["_list"].([]any) {
		if m := it.(map[string]any); m["name"] == "newcomer" {
			if r := reindex("", m["id"].(float64)); r["_status"] != float64(409) || errorCode(r) != "not_bound" {
				t.Errorf("reindex of an unbound store: %v", r)
			}
		}
	}

	// A store wider than any index covers: not a case for the action.
	if _, err := v.store.DB().Exec(context.Background(), "UPDATE rag_stores SET dimensions = 5000 WHERE name = 'newcomer'"); err != nil {
		t.Fatal(err)
	}
	var wideID float64
	for _, it := range v.call("GET", "/admin/api/rag-stores", nil, "")["_list"].([]any) {
		if m := it.(map[string]any); m["name"] == "newcomer" {
			wideID = m["id"].(float64)
			if m["vector_index"] != "not_indexable" {
				t.Errorf("vector_index of a 5000-dimension store = %v, want not_indexable", m["vector_index"])
			}
		}
	}
	if r := reindex("", wideID); r["_status"] != float64(409) || errorCode(r) != "not_indexable" {
		t.Errorf("reindex of a 5000-dimension store: %v", r)
	}
}

// A width of 2000 or less never needs the action.
func TestReindexOfAPlainWidthIsRefused(t *testing.T) {
	up := mockUpstream(t)
	defer up.Close()
	e := newEnv(t, testdb.Config(t))
	c := e.call("POST", "/admin/api/models", map[string]any{"name": "narrow", "provider_type": "custom_openai",
		"base_url": up.URL + "/v1", "api_key": "secret", "model_name": "emb"}, "")
	rs := e.call("POST", "/admin/api/rag-stores", map[string]any{"name": "docs", "embedding_connection_id": c["id"],
		"chunk_size": 500, "chunk_overlap": 50, "top_k": 3}, "")
	if rs["_status"] != float64(201) {
		t.Fatalf("create: %v", rs)
	}
	doc := e.upload(int64(rs["id"].(float64)), "a.txt", "apple banana")
	e.waitReady(int64(doc["id"].(float64)))
	if got := e.call("GET", fmt.Sprintf("/admin/api/rag-stores/%d", int64(rs["id"].(float64))), nil, ""); got["vector_index"] != "hnsw" {
		t.Errorf("vector_index of a plain store = %v, want hnsw", got["vector_index"])
	}
	r := e.call("POST", fmt.Sprintf("/admin/api/rag-stores/%d/reindex", int64(rs["id"].(float64))), nil, "")
	if r["_status"] != float64(409) || errorCode(r) != "already_indexed" {
		t.Errorf("reindex of a plain store: %v", r)
	}
}

// When the width probe cannot answer, the save goes ahead and the ingest is
// the last check: the document fails with a message that says what to do.
func TestIngestFailsWhenTheWidthCannotBeIndexed(t *testing.T) {
	probeless := func(a *admin.Admin) {
		a.EmbeddingWidth = func(context.Context, *store.ModelConnection) (int, error) { return 0, errors.New("no probe") }
	}
	docError := func(e *env, storeID float64) string {
		t.Helper()
		doc := e.upload(int64(storeID), "a.txt", "some text")
		id := int64(doc["id"].(float64))
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			d := e.call("GET", fmt.Sprintf("/admin/api/documents/%d", id), nil, "")
			switch d["status"] {
			case "failed":
				return fmt.Sprint(d["error"])
			case "ready":
				t.Fatal("the document became ready")
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatal("the document never failed")
		return ""
	}
	newStore := func(e *env, dims int, name string) float64 {
		t.Helper()
		up := wideEmbeddingUpstream(dims)
		t.Cleanup(up.Close)
		c := e.call("POST", "/admin/api/models", map[string]any{"name": name + "-emb", "provider_type": "custom_openai",
			"base_url": up.URL + "/v1", "api_key": "secret", "model_name": "emb"}, "")
		rs := e.call("POST", "/admin/api/rag-stores", map[string]any{"name": name, "embedding_connection_id": c["id"],
			"chunk_size": 500, "chunk_overlap": 50, "top_k": 3}, "")
		if rs["_status"] != float64(201) {
			t.Fatalf("create %s: %v", name, rs)
		}
		return rs["id"].(float64)
	}

	e := newEnvWith(t, testdb.Config(t), true, probeless)
	if !e.store.Caps().SupportsHalfvec() {
		t.Skipf("pgvector %q has no halfvec", e.store.Caps().PgvectorVersion)
	}
	msg := docError(e, newStore(e, 4097, "too-wide"))
	for _, part := range []string{"4097", "4000", "binary quantization", "v0.9"} {
		if !strings.Contains(msg, part) {
			t.Errorf("failed document error should mention %q: %q", part, msg)
		}
	}

	// A width that is bound and not indexed turns a new store's first ingest away.
	legacy := newStore(e, 2500, "legacy")
	e.waitReady(int64(e.upload(int64(legacy), "a.txt", "some text")["id"].(float64)))
	if _, err := e.store.DB().Exec(context.Background(), "DROP INDEX chunk_embeddings_2500_hnsw_half"); err != nil {
		t.Fatal(err)
	}
	msg = docError(e, newStore(e, 2500, "newcomer"))
	if !strings.Contains(msg, "reindex") || !strings.Contains(msg, "2500") {
		t.Errorf("failed document error should tell to reindex the width: %q", msg)
	}
}

// Starting the reindex twice while it runs, and uploading a document into the
// width meanwhile, leaves the one build alone: both calls answer 202 with the
// running build and the upload becomes ready.
func TestReindexTwiceAndUploadWhileBuilding(t *testing.T) {
	v := newVectorEnv(t, 2500)
	a := v.createStore("legacy")
	aID := a["id"].(float64)
	v.waitReady(int64(v.upload(int64(aID), "a.txt", "some text")["id"].(float64)))
	ctx := context.Background()
	if _, err := v.store.DB().Exec(ctx, "DROP INDEX chunk_embeddings_2500_hnsw_half"); err != nil {
		t.Fatal(err)
	}
	// Keep the build waiting for old snapshots until the test lets go.
	tx, err := v.store.DB().BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT 1 FROM chunk_embeddings_2500 LIMIT 1"); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/admin/api/rag-stores/%d/reindex", int64(aID))
	if r := v.call("POST", path, nil, ""); r["_status"] != float64(202) {
		t.Fatalf("first reindex: %v", r)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		var n int
		if err := v.store.DB().QueryRow(ctx, `SELECT COUNT(*) FROM pg_stat_progress_create_index
			WHERE phase LIKE 'waiting for old snapshots%'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the build never reached its wait")
		}
		time.Sleep(20 * time.Millisecond)
	}
	r := v.call("POST", path, nil, "")
	if r["_status"] != float64(202) || r["vector_index"] != "building" {
		t.Fatalf("second reindex while building: %v", r)
	}
	if got := v.storeIndex(aID); got["vector_index"] != "building" || got["vector_index_progress"] == nil {
		t.Errorf("store while building: vector_index=%v progress=%v", got["vector_index"], got["vector_index_progress"])
	}
	// The ingester is the same code the replicas run.
	v.waitReady(int64(v.upload(int64(aID), "b.txt", "more text")["id"].(float64)))

	_ = tx.Rollback(ctx)
	v.waitVectorIndex(aID, "hnsw_halfvec")
}
