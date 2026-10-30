package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ragmux/ragmux/internal/testdb"
)

// rerankUpstream is a Cohere-compatible rerank server. It answers in a
// deliberately unsorted order, so a test can tell the gateway sorted the
// results itself, and reports 42 input tokens.
func rerankUpstream(calls *atomic.Int64, lastModel *atomic.Value) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/rerank" {
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model     string   `json:"model"`
			Documents []string `json:"documents"`
			TopN      int      `json:"top_n"`
		}
		_ = json.Unmarshal(body, &req)
		if lastModel != nil {
			lastModel.Store(req.Model)
		}
		scores := []float64{0.2, 0.9, 0.5, 0.7, 0.1}
		var results []map[string]any
		for i := range req.Documents {
			results = append(results, map[string]any{"index": i, "relevance_score": scores[i%len(scores)]})
		}
		if req.TopN > 0 && req.TopN < len(results) {
			results = results[:req.TopN]
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "r1", "results": results,
			"meta": map[string]any{"tokens": map[string]int{"input_tokens": 42, "output_tokens": 0}}})
	}))
}

func TestRerankEndpoint(t *testing.T) {
	var rerankCalls atomic.Int64
	var lastModel atomic.Value
	rr := rerankUpstream(&rerankCalls, &lastModel)
	defer rr.Close()
	up := mockUpstream(t)
	defer up.Close()
	e := newEnv(t, testdb.Config(t))

	chat := e.call("POST", "/admin/api/models", map[string]any{"name": "chat", "provider_type": "custom_openai",
		"base_url": up.URL + "/v1", "api_key": "k", "model_name": "chat-model"}, "")
	chatID := int64(chat["id"].(float64))
	co := e.call("POST", "/admin/api/models", map[string]any{"name": "cohere", "provider_type": "cohere_rerank",
		"base_url": rr.URL, "api_key": "sk-co", "model_name": "rerank-v3.5"}, "")
	if co["_status"] != float64(201) {
		t.Fatalf("rerank connection: %v", co)
	}
	coID := int64(co["id"].(float64))
	rs := e.call("POST", "/admin/api/rag-stores", map[string]any{"name": "s", "embedding_connection_id": chatID,
		"chunk_size": 200, "chunk_overlap": 0, "top_k": 2, "rerank": true,
		"rerank_backend": "cohere", "rerank_connection_id": coID}, "")
	if rs["_status"] != float64(201) {
		t.Fatalf("create store: %v", rs)
	}
	proj := e.call("POST", "/admin/api/projects", map[string]any{"name": "app", "model_connection_id": chatID,
		"rag_store_id": int64(rs["id"].(float64))}, "")
	if proj["_status"] != float64(201) {
		t.Fatalf("create project: %v", proj)
	}
	pid := int64(proj["project"].(map[string]any)["id"].(float64))
	mint := func(scopes ...string) string {
		k := e.call("POST", "/admin/api/keys", map[string]any{"kind": "gateway", "name": fmt.Sprint(scopes),
			"scopes": scopes, "project_ids": []int64{pid}, "default_project_id": pid}, "")
		if k["_status"] != float64(201) {
			t.Fatalf("mint key %v: %v", scopes, k)
		}
		return k["api_key"].(string)
	}
	key := mint("rerank")

	// A Cohere-format request through the store's implicit route.
	body := map[string]any{"model": "rerank-v3.5", "query": "which fruit is yellow",
		"documents": []string{"apples are red", "bananas are yellow", "cherries are sweet", "durians smell"}}
	res, hdr := e.callRaw("POST", "/v1/rerank", body, key)
	if res["_status"] != float64(200) {
		t.Fatalf("rerank: %v", res)
	}
	results, _ := res["results"].([]any)
	if len(results) != 4 {
		t.Fatalf("results: %v", res["results"])
	}
	prev := 2.0
	for i, it := range results {
		m := it.(map[string]any)
		score, ok := m["relevance_score"].(float64)
		if !ok || m["index"] == nil || score > prev {
			t.Errorf("result %d out of order or malformed: %v", i, m)
		}
		prev = score
	}
	if first := results[0].(map[string]any); first["index"] != float64(1) || first["relevance_score"] != 0.9 {
		t.Errorf("best result: %v", first)
	}
	row, found := e.logRow(hdr.Get("X-Request-Id"))
	if !found {
		t.Fatal("no request_logs row for the rerank call")
	}
	if row.endpoint != "rerank" || row.status != 200 || row.promptTokens != 42 || row.estimated || row.modelName != "rerank-v3.5" {
		t.Errorf("log row: %+v", row)
	}
	if meta, _ := res["meta"].(map[string]any); meta == nil || meta["tokens"].(map[string]any)["input_tokens"] != float64(42) {
		t.Errorf("meta: %v", res["meta"])
	}

	// top_n cuts the list; an empty model means the store's rerank connection;
	// return_documents echoes the text.
	cut := e.call("POST", "/v1/rerank", map[string]any{"query": "q", "documents": []string{"a", "b", "c"},
		"top_n": 2, "return_documents": true}, key)
	cutResults, _ := cut["results"].([]any)
	if cut["_status"] != float64(200) || len(cutResults) != 2 {
		t.Fatalf("top_n 2: %v", cut)
	}
	if doc, _ := cutResults[0].(map[string]any)["document"].(map[string]any); doc == nil || doc["text"] == "" {
		t.Errorf("return_documents: %v", cutResults[0])
	}

	// A kind=rerank route answers to its own name and serves its own model.
	rt := e.call("POST", fmt.Sprintf("/admin/api/projects/%d/routes", pid), map[string]any{"name": "fast-rerank",
		"kind": "rerank", "targets": []any{map[string]any{"connection_id": coID, "model": "rerank-lite"}}}, "")
	if rt["_status"] != float64(201) {
		t.Fatalf("create rerank route: %v", rt)
	}
	routed, rh := e.callRaw("POST", "/v1/rerank", map[string]any{"model": "fast-rerank", "query": "q",
		"documents": []string{"a", "b"}}, key)
	if routed["_status"] != float64(200) || lastModel.Load() != "rerank-lite" {
		t.Errorf("routed call: %v upstream model %v", routed, lastModel.Load())
	}
	if row, ok := e.logRow(rh.Get("X-Request-Id")); !ok || row.route != "fast-rerank" || row.endpoint != "rerank" {
		t.Errorf("routed log row: %+v found=%v", row, ok)
	}

	// An unknown model is model_not_found and never reaches the upstream.
	before := rerankCalls.Load()
	miss := e.call("POST", "/v1/rerank", map[string]any{"model": "no-such-model", "query": "q", "documents": []string{"a"}}, key)
	if errObj, _ := miss["error"].(map[string]any); miss["_status"] != float64(404) || errObj["code"] != "model_not_found" {
		t.Errorf("unknown model: %v", miss)
	}
	if rerankCalls.Load() != before {
		t.Errorf("unknown model reached the upstream")
	}

	// Validation.
	for name, b := range map[string]map[string]any{
		"no query":     {"documents": []string{"a"}},
		"no documents": {"query": "q"},
		"empty list":   {"query": "q", "documents": []string{}},
		"bad top_n":    {"query": "q", "documents": []string{"a"}, "top_n": 0},
		"bad document": {"query": "q", "documents": []any{5}},
	} {
		if r := e.call("POST", "/v1/rerank", b, key); r["_status"] != float64(400) {
			t.Errorf("%s: want 400, got %v", name, r)
		}
	}

	// Scope and authentication.
	if r := e.call("POST", "/v1/rerank", body, mint("chat")); r["_status"] != float64(403) {
		t.Errorf("key without the rerank scope: %v", r)
	}
	if r := e.call("POST", "/v1/chat/completions", map[string]any{"messages": []any{map[string]any{"role": "user", "content": "x"}}}, key); r["_status"] != float64(403) {
		t.Errorf("rerank-only key on chat: %v", r)
	}
	if r := e.call("POST", "/v1/rerank", body, "sk-proj-bogus"); r["_status"] != float64(401) {
		t.Errorf("bad key: %v", r)
	}
}

// A project whose store has no rerank connection has no implicit rerank
// route: only a named route or nothing (ADR-018).
func TestRerankWithoutStoreConnection(t *testing.T) {
	up := mockUpstream(t)
	defer up.Close()
	e := newEnv(t, testdb.Config(t))
	c := e.call("POST", "/admin/api/models", map[string]any{"name": "chat", "provider_type": "custom_openai",
		"base_url": up.URL + "/v1", "api_key": "k", "model_name": "chat-model"}, "")
	proj := e.call("POST", "/admin/api/projects", map[string]any{"name": "bare", "model_connection_id": int64(c["id"].(float64))}, "")
	r := e.call("POST", "/v1/rerank", map[string]any{"model": "chat-model", "query": "q", "documents": []string{"a", "b"}}, proj["api_key"].(string))
	if errObj, _ := r["error"].(map[string]any); r["_status"] != float64(404) || errObj["code"] != "model_not_found" {
		t.Errorf("no store connection, no route: %v", r)
	}
}

// rerankChatUpstream is a chat server that answers rerank prompts with a
// ranking and counts the prompts it saw.
func rerankChatUpstream(calls *atomic.Int64) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "r", "model": "m",
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "[2, 1]"}, "finish_reason": "stop"}},
			"usage":   map[string]int{"prompt_tokens": 30, "completion_tokens": 4, "total_tokens": 34}})
	}))
}

// The admin search reranks with the connection the store names, not with the
// chat model of whichever project happens to use the store first. A store that
// names none is searched without a rerank and the response says so.
func TestAdminSearchUsesStoreRerankConnection(t *testing.T) {
	var callsA, callsB, callsC atomic.Int64
	upA, upB, upC := rerankChatUpstream(&callsA), rerankChatUpstream(&callsB), rerankChatUpstream(&callsC)
	defer upA.Close()
	defer upB.Close()
	defer upC.Close()
	emb := mockUpstream(t)
	defer emb.Close()
	e := newEnv(t, testdb.Config(t))

	mk := func(name, url, model string) int64 {
		c := e.call("POST", "/admin/api/models", map[string]any{"name": name, "provider_type": "custom_openai",
			"base_url": url + "/v1", "api_key": "k", "model_name": model}, "")
		if c["_status"] != float64(201) {
			t.Fatalf("connection %s: %v", name, c)
		}
		return int64(c["id"].(float64))
	}
	embID := mk("embed", emb.URL, "embed-model")
	chatA, chatB, chatC := mk("chat-a", upA.URL, "model-a"), mk("chat-b", upB.URL, "model-b"), mk("chat-c", upC.URL, "model-c")

	rs := e.call("POST", "/admin/api/rag-stores", map[string]any{"name": "fruit", "embedding_connection_id": embID,
		"chunk_size": 200, "chunk_overlap": 0, "top_k": 2, "rerank": true, "rerank_connection_id": chatB}, "")
	if rs["_status"] != float64(201) || rs["rerank_connection_id"] != float64(chatB) {
		t.Fatalf("create store: %v", rs)
	}
	storeID := int64(rs["id"].(float64))
	doc := e.upload(storeID, "fruits.md", "# Apple\n\nApples are red and crunchy. They grow on trees in temperate climates and store well.\n\n# Banana\n\nBananas are yellow and soft. They grow in bunches in tropical regions and ripen fast.\n\n# Cherry\n\nCherries are small and sweet. They are harvested in early summer and eaten fresh.")
	e.waitReady(int64(doc["id"].(float64)))
	// Two projects share the store, each with its own chat model.
	for name, conn := range map[string]int64{"one": chatA, "two": chatC} {
		if p := e.call("POST", "/admin/api/projects", map[string]any{"name": name, "model_connection_id": conn, "rag_store_id": storeID}, ""); p["_status"] != float64(201) {
			t.Fatalf("project %s: %v", name, p)
		}
	}

	path := fmt.Sprintf("/admin/api/rag-stores/%d/search", storeID)
	res := e.call("POST", path, map[string]any{"query": "tell me about fruit"}, "")
	if res["_status"] != float64(200) || res["reranked"] != true || res["rerank_skipped"] != false {
		t.Fatalf("search: %v", res)
	}
	if callsB.Load() != 1 || callsA.Load() != 0 || callsC.Load() != 0 {
		t.Errorf("rerank calls: a=%d b=%d c=%d, want only b", callsA.Load(), callsB.Load(), callsC.Load())
	}

	// Without a rerank connection the search runs unreranked and says so.
	upd := e.call("PUT", fmt.Sprintf("/admin/api/rag-stores/%d", storeID), map[string]any{"name": "fruit", "embedding_connection_id": embID,
		"chunk_size": 200, "chunk_overlap": 0, "top_k": 2, "rerank": true}, "")
	if upd["_status"] != float64(200) || upd["rerank_connection_id"] != nil {
		t.Fatalf("clear rerank connection: %v", upd)
	}
	res = e.call("POST", path, map[string]any{"query": "tell me about fruit"}, "")
	if res["_status"] != float64(200) || res["reranked"] != false || res["rerank_skipped"] != true || res["rerank_skipped_reason"] == "" ||
		res["rerank_fallback"] != false {
		t.Errorf("search without a rerank connection: %v", res)
	}
	if callsA.Load() != 0 || callsB.Load() != 1 || callsC.Load() != 0 {
		t.Errorf("a project's chat model was used: a=%d b=%d c=%d", callsA.Load(), callsB.Load(), callsC.Load())
	}
}

// An llm-backend store names a chat model as its rerank connection; /v1/rerank
// serves it through the implicit route, ranks by that model's answer and
// counts the model's tokens on its own log row.
func TestRerankThroughChatModel(t *testing.T) {
	var calls atomic.Int64
	up := rerankChatUpstream(&calls)
	defer up.Close()
	emb := mockUpstream(t)
	defer emb.Close()
	e := newEnv(t, testdb.Config(t))
	mk := func(name, url, model string) int64 {
		c := e.call("POST", "/admin/api/models", map[string]any{"name": name, "provider_type": "custom_openai",
			"base_url": url + "/v1", "api_key": "k", "model_name": model}, "")
		return int64(c["id"].(float64))
	}
	embID, chatID := mk("embed", emb.URL, "embed-model"), mk("chat", up.URL, "rank-model")
	rs := e.call("POST", "/admin/api/rag-stores", map[string]any{"name": "s", "embedding_connection_id": embID,
		"chunk_size": 200, "chunk_overlap": 0, "top_k": 2, "rerank": true, "rerank_connection_id": chatID}, "")
	if rs["_status"] != float64(201) {
		t.Fatalf("create store: %v", rs)
	}
	proj := e.call("POST", "/admin/api/projects", map[string]any{"name": "app", "model_connection_id": chatID,
		"rag_store_id": int64(rs["id"].(float64))}, "")
	pid := int64(proj["project"].(map[string]any)["id"].(float64))
	k := e.call("POST", "/admin/api/keys", map[string]any{"kind": "gateway", "name": "rr", "scopes": []string{"rerank"},
		"project_ids": []int64{pid}, "default_project_id": pid}, "")

	res, hdr := e.callRaw("POST", "/v1/rerank", map[string]any{"query": "which is best",
		"documents": []string{"first passage", "second passage"}}, k["api_key"].(string))
	results, _ := res["results"].([]any)
	if res["_status"] != float64(200) || len(results) != 2 || results[0].(map[string]any)["index"] != float64(1) {
		t.Fatalf("rerank through a chat model: %v", res)
	}
	if a, b := results[0].(map[string]any)["relevance_score"].(float64), results[1].(map[string]any)["relevance_score"].(float64); a <= b {
		t.Errorf("scores must descend: %v then %v", a, b)
	}
	row, found := e.logRow(hdr.Get("X-Request-Id"))
	if !found || row.endpoint != "rerank" || row.promptTokens != 30 || row.completionTokens != 4 || row.modelName != "rank-model" {
		t.Errorf("log row: %+v found=%v", row, found)
	}
}

// The rerank_backend and rerank_connection_id overrides on the admin search
// choose which upstream receives the store's chunk text, so they are open only
// to callers who may edit the store. A viewer (or a management key without the
// write scope) searches with the store's saved connection; the override is
// ignored and reported in a Warning header and in the response, not rejected.
func TestAdminSearchRerankOverrideNeedsEditor(t *testing.T) {
	var callsSaved, callsOther atomic.Int64
	upSaved, upOther := rerankChatUpstream(&callsSaved), rerankChatUpstream(&callsOther)
	defer upSaved.Close()
	defer upOther.Close()
	emb := mockUpstream(t)
	defer emb.Close()
	e := newEnv(t, testdb.Config(t))

	mk := func(name, url, model string) int64 {
		c := e.call("POST", "/admin/api/models", map[string]any{"name": name, "provider_type": "custom_openai",
			"base_url": url + "/v1", "api_key": "k", "model_name": model}, "")
		if c["_status"] != float64(201) {
			t.Fatalf("connection %s: %v", name, c)
		}
		return int64(c["id"].(float64))
	}
	embID := mk("embed", emb.URL, "embed-model")
	saved, other := mk("saved", upSaved.URL, "model-saved"), mk("other", upOther.URL, "model-other")
	rs := e.call("POST", "/admin/api/rag-stores", map[string]any{"name": "fruit", "embedding_connection_id": embID,
		"chunk_size": 200, "chunk_overlap": 0, "top_k": 2, "rerank": true, "rerank_connection_id": saved}, "")
	if rs["_status"] != float64(201) {
		t.Fatalf("create store: %v", rs)
	}
	storeID := int64(rs["id"].(float64))
	doc := e.upload(storeID, "fruits.md", "# Apple\n\nApples are red and crunchy. They grow on trees in temperate climates and store well.\n\n# Banana\n\nBananas are yellow and soft. They grow in bunches in tropical regions and ripen fast.\n\n# Cherry\n\nCherries are small and sweet. They are harvested in early summer and eaten fresh.")
	e.waitReady(int64(doc["id"].(float64)))

	e.call("POST", "/admin/api/users", map[string]any{"username": "vw", "password": "viewer-pass-1", "role": "viewer"}, "")
	e.call("POST", "/admin/api/users", map[string]any{"username": "ed", "password": "editor-pass-1", "role": "editor"}, "")
	viewer, editor := e.login("vw", "viewer-pass-1"), e.login("ed", "editor-pass-1")
	keyN := 0
	mgmtKey := func(scopes ...string) string {
		keyN++
		k := e.call("POST", "/admin/api/keys", map[string]any{"kind": "management", "name": fmt.Sprintf("k%d", keyN), "scopes": scopes}, "")
		if k["_status"] != float64(201) {
			t.Fatalf("management key: %v", k)
		}
		return k["api_key"].(string)
	}
	path := fmt.Sprintf("/admin/api/rag-stores/%d/search", storeID)
	reset := func() { callsSaved.Store(0); callsOther.Store(0) }

	ignored := map[string]struct {
		bearer string
		body   map[string]any
		fields []any
	}{
		"viewer connection":     {viewer, map[string]any{"rerank_connection_id": other}, []any{"rerank_connection_id"}},
		"viewer backend":        {viewer, map[string]any{"rerank_backend": "cohere"}, []any{"rerank_backend"}},
		"viewer clear":          {viewer, map[string]any{"rerank_connection_id": 0}, []any{"rerank_connection_id"}},
		"viewer both":           {viewer, map[string]any{"rerank_backend": "llm", "rerank_connection_id": other}, []any{"rerank_backend", "rerank_connection_id"}},
		"viewer bogus value":    {viewer, map[string]any{"rerank_backend": "nope", "rerank_connection_id": 99999}, []any{"rerank_backend", "rerank_connection_id"}},
		"read-only key":         {mgmtKey("read"), map[string]any{"rerank_connection_id": other}, []any{"rerank_connection_id"}},
		"admin owner, read key": {mgmtKey("read"), map[string]any{"rerank_backend": "llm", "rerank_connection_id": other}, []any{"rerank_backend", "rerank_connection_id"}},
	}
	for name, c := range ignored {
		reset()
		c.body["query"] = "tell me about fruit"
		res, hdr := e.callRaw("POST", path, c.body, c.bearer)
		if res["_status"] != float64(200) || res["reranked"] != true || res["rerank_skipped"] != false {
			t.Errorf("%s: search: %v", name, res)
			continue
		}
		if callsSaved.Load() != 1 || callsOther.Load() != 0 {
			t.Errorf("%s: saved=%d other=%d, want only the saved connection", name, callsSaved.Load(), callsOther.Load())
		}
		if w := hdr.Values("Warning"); len(w) != 1 || !strings.HasPrefix(w[0], `299 ragmux "`) || !strings.Contains(w[0], "ignored") {
			t.Errorf("%s: Warning header = %q", name, w)
		}
		if got, _ := res["rerank_overrides_ignored"].([]any); fmt.Sprint(got) != fmt.Sprint(c.fields) {
			t.Errorf("%s: rerank_overrides_ignored = %v, want %v", name, res["rerank_overrides_ignored"], c.fields)
		}
	}

	// No override, no noise: a viewer's plain search uses the saved connection
	// and carries no Warning.
	reset()
	res, hdr := e.callRaw("POST", path, map[string]any{"query": "tell me about fruit"}, viewer)
	if res["_status"] != float64(200) || callsSaved.Load() != 1 || callsOther.Load() != 0 || len(hdr.Values("Warning")) != 0 {
		t.Errorf("viewer plain search: %v saved=%d other=%d warning=%q", res, callsSaved.Load(), callsOther.Load(), hdr.Values("Warning"))
	}
	if got, _ := res["rerank_overrides_ignored"].([]any); len(got) != 0 {
		t.Errorf("viewer plain search reported ignored overrides: %v", got)
	}

	// Callers who can edit the store keep the override.
	for name, bearer := range map[string]string{"editor": editor, "admin": "", "key with write scope": mgmtKey("read", "write")} {
		reset()
		res, hdr := e.callRaw("POST", path, map[string]any{"query": "tell me about fruit", "rerank_connection_id": other}, bearer)
		if res["_status"] != float64(200) || res["reranked"] != true {
			t.Errorf("%s: search: %v", name, res)
			continue
		}
		if callsOther.Load() != 1 || callsSaved.Load() != 0 {
			t.Errorf("%s: saved=%d other=%d, want the override used", name, callsSaved.Load(), callsOther.Load())
		}
		if len(hdr.Values("Warning")) != 0 {
			t.Errorf("%s: unexpected Warning %q", name, hdr.Values("Warning"))
		}
	}
	// An editor's invalid override is still a 400: validation applies to those who may override.
	if r := e.call("POST", path, map[string]any{"query": "q", "rerank_backend": "nope"}, editor); r["_status"] != float64(400) {
		t.Errorf("editor with a bad rerank_backend: %v", r)
	}
}
