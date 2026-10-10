package e2e

import (
	"strings"
	"testing"

	"github.com/ragmux/ragmux/internal/testdb"
)

func TestRetrieveEndpoint(t *testing.T) {
	up := mockUpstream(t)
	defer up.Close()
	e := newEnv(t, testdb.Config(t))

	conn := e.call("POST", "/admin/api/models", map[string]any{"name": "mock", "provider_type": "custom_openai",
		"base_url": up.URL + "/v1", "api_key": "k", "model_name": "mock-model"}, "")
	connID := int64(conn["id"].(float64))
	rs := e.call("POST", "/admin/api/rag-stores", map[string]any{"name": "fruit", "embedding_connection_id": connID,
		"chunk_size": 200, "chunk_overlap": 0, "top_k": 1}, "")
	storeID := int64(rs["id"].(float64))
	doc := e.upload(storeID, "fruits.md", "# Apple\n\nApples are red and crunchy. They grow on trees in temperate climates and store well.\n\n# Banana\n\nBananas are yellow and soft. They grow in bunches in tropical regions and ripen fast.\n\n# Cherry\n\nCherries are small and sweet. They are harvested in early summer and eaten fresh.")
	if doc["_status"] != float64(202) {
		t.Fatalf("upload: %v", doc)
	}
	e.waitReady(int64(doc["id"].(float64)))
	proj := e.call("POST", "/admin/api/projects", map[string]any{"name": "app", "model_connection_id": connID, "rag_store_id": storeID}, "")
	if proj["_status"] != float64(201) {
		t.Fatalf("create project: %v", proj)
	}
	pid := int64(proj["project"].(map[string]any)["id"].(float64))

	// A viewer, a member of the project, mints a key with the retrieve scope.
	// /v1 does not read the role: the scope decides.
	vw := e.call("POST", "/admin/api/users", map[string]any{"username": "reader", "password": "reader-pass-1",
		"role": "viewer", "project_ids": []int64{pid}}, "")
	if vw["_status"] != float64(201) {
		t.Fatalf("create viewer: %v", vw)
	}
	viewerSession := e.login("reader", "reader-pass-1")
	mint := func(scopes ...string) string {
		k := e.call("POST", "/admin/api/keys", map[string]any{"kind": "gateway", "name": strings.Join(scopes, "-"),
			"scopes": scopes, "project_ids": []int64{pid}, "default_project_id": pid}, viewerSession)
		if k["_status"] != float64(201) {
			t.Fatalf("viewer mints key %v: %v", scopes, k)
		}
		return k["api_key"].(string)
	}
	retrieveKey := mint("retrieve")

	res, hdr := e.callRaw("POST", "/v1/retrieve", map[string]any{"query": "tell me about banana"}, retrieveKey)
	if res["_status"] != float64(200) {
		t.Fatalf("viewer retrieve: %v", res)
	}
	hits, ok := res["hits"].([]any)
	if !ok || len(hits) != 1 {
		t.Fatalf("hits: %v", res["hits"])
	}
	hit := hits[0].(map[string]any)
	meta := hit["metadata"].(map[string]any)
	if !strings.Contains(hit["content"].(string), "Banana") || meta["filename"] != "fruits.md" || meta["section"] == nil {
		t.Errorf("hit: %v", hit)
	}
	if _, ok := hit["score"].(float64); !ok || hit["chunk_id"] == nil || hit["document_id"] == nil {
		t.Errorf("hit scores and ids: %v", hit)
	}
	if res["mode"] == "" || res["mode"] == nil || res["backend"] == nil || res["reranked"] != false {
		t.Errorf("mode and backend: %v", res)
	}
	if u, _ := res["usage"].(map[string]any); u == nil || u["prompt_tokens"].(float64) <= 0 {
		t.Errorf("usage: %v", res["usage"])
	}

	// top_k overrides the store's top_k within bounds.
	if wide := e.call("POST", "/v1/retrieve", map[string]any{"query": "fruit", "top_k": 3}, retrieveKey); len(wide["hits"].([]any)) < 2 {
		t.Errorf("top_k 3: %v", wide["hits"])
	}

	// The request is accounted like any other.
	row, found := e.logRow(hdr.Get("X-Request-Id"))
	if !found {
		t.Fatal("no request_logs row for the retrieve call")
	}
	if row.endpoint != "retrieve" || row.status != 200 || row.promptTokens <= 0 || row.modelName != "mock-model" {
		t.Errorf("log row: %+v", row)
	}
	var ragHits int
	var ragUsed bool
	if err := e.store.DB().QueryRow(t.Context(), `SELECT rag_used, rag_hits FROM request_logs WHERE request_id = $1`,
		hdr.Get("X-Request-Id")).Scan(&ragUsed, &ragHits); err != nil || !ragUsed || ragHits != 1 {
		t.Errorf("rag columns: used=%v hits=%d err=%v", ragUsed, ragHits, err)
	}

	// Scope: a key without retrieve is refused, so is one with only other scopes.
	for _, k := range []string{mint("chat"), mint("embeddings")} {
		if r := e.call("POST", "/v1/retrieve", map[string]any{"query": "banana"}, k); r["_status"] != float64(403) {
			t.Errorf("key without the retrieve scope: %v", r)
		}
	}
	// And a retrieve-only key does not open the other endpoints.
	if r := e.call("POST", "/v1/embeddings", map[string]any{"input": "x"}, retrieveKey); r["_status"] != float64(403) {
		t.Errorf("retrieve key on embeddings: %v", r)
	}

	// Validation.
	for name, body := range map[string]map[string]any{
		"no query":    {},
		"blank query": {"query": "   "},
		"negative k":  {"query": "x", "top_k": -1},
		"huge k":      {"query": "x", "top_k": 1000},
	} {
		if r := e.call("POST", "/v1/retrieve", body, retrieveKey); r["_status"] != float64(400) {
			t.Errorf("%s: want 400, got %v", name, r)
		}
	}
	if r := e.call("POST", "/v1/retrieve", map[string]any{"query": "x"}, "sk-proj-bogus"); r["_status"] != float64(401) {
		t.Errorf("bad key: %v", r)
	}

	// A project with no RAG store has nothing to search.
	bare := e.call("POST", "/admin/api/projects", map[string]any{"name": "bare", "model_connection_id": connID}, "")
	r := e.call("POST", "/v1/retrieve", map[string]any{"query": "banana"}, bare["api_key"].(string))
	if errObj, _ := r["error"].(map[string]any); r["_status"] != float64(400) || errObj["code"] != "no_rag_store" {
		t.Errorf("project without a store: %v", r)
	}
}

// An LLM rerank is a chat call on the project's chat connection. A retrieve-only
// key opens it, so its tokens are counted on the request row and in the
// response usage, not just the query's embedding tokens.
func TestRetrieveCountsRerankTokens(t *testing.T) {
	up := mockUpstream(t)
	defer up.Close()
	e := newEnv(t, testdb.Config(t))

	conn := e.call("POST", "/admin/api/models", map[string]any{"name": "mock", "provider_type": "custom_openai",
		"base_url": up.URL + "/v1", "api_key": "k", "model_name": "mock-model"}, "")
	connID := int64(conn["id"].(float64))
	rs := e.call("POST", "/admin/api/rag-stores", map[string]any{"name": "fruit", "embedding_connection_id": connID,
		"chunk_size": 200, "chunk_overlap": 0, "top_k": 2, "rerank": true}, "")
	if rs["_status"] != float64(201) {
		t.Fatalf("create store: %v", rs)
	}
	storeID := int64(rs["id"].(float64))
	doc := e.upload(storeID, "fruits.md", "# Apple\n\nApples are red and crunchy. They grow on trees in temperate climates and store well.\n\n# Banana\n\nBananas are yellow and soft. They grow in bunches in tropical regions and ripen fast.\n\n# Cherry\n\nCherries are small and sweet. They are harvested in early summer and eaten fresh.")
	e.waitReady(int64(doc["id"].(float64)))
	proj := e.call("POST", "/admin/api/projects", map[string]any{"name": "app", "model_connection_id": connID, "rag_store_id": storeID}, "")
	if proj["_status"] != float64(201) {
		t.Fatalf("create project: %v", proj)
	}
	pid := int64(proj["project"].(map[string]any)["id"].(float64))
	k := e.call("POST", "/admin/api/keys", map[string]any{"kind": "gateway", "name": "ret", "scopes": []string{"retrieve"},
		"project_ids": []int64{pid}, "default_project_id": pid}, "")
	key := k["api_key"].(string)

	res, hdr := e.callRaw("POST", "/v1/retrieve", map[string]any{"query": "tell me about fruit"}, key)
	if res["_status"] != float64(200) || res["reranked"] != true {
		t.Fatalf("reranked retrieve: %v", res)
	}
	row, found := e.logRow(hdr.Get("X-Request-Id"))
	if !found {
		t.Fatal("no request_logs row")
	}
	if row.completionTokens <= 0 || !row.estimated {
		t.Errorf("rerank tokens missing from the row: %+v", row)
	}
	usage := res["usage"].(map[string]any)
	if int(usage["total_tokens"].(float64)) != row.promptTokens+row.completionTokens {
		t.Errorf("response usage %v does not match the row %+v", usage, row)
	}
}
