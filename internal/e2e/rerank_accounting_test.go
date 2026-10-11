package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ragmux/ragmux/internal/gateway"
	"github.com/ragmux/ragmux/internal/pricing"
	"github.com/ragmux/ragmux/internal/provider"
	"github.com/ragmux/ragmux/internal/testdb"
)

// rankingUpstream is an OpenAI-compatible server for the rerank accounting
// tests. A rerank prompt is answered with the current value of reply and
// 30+4 tokens; any other chat request gets "ok" and 10+2 tokens; embeddings
// are the keyword vectors the other tests use.
func rankingUpstream(reply *atomic.Value) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/v1/embeddings":
			var req struct {
				Input []string `json:"input"`
			}
			_ = json.Unmarshal(body, &req)
			data := make([]map[string]any, len(req.Input))
			for i, in := range req.Input {
				data[i] = map[string]any{"index": i, "embedding": keywordVec(in)}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
		case "/v1/chat/completions":
			var req provider.ChatRequest
			_ = json.Unmarshal(body, &req)
			lastUser := ""
			for _, m := range req.Messages {
				if m.Role == "user" {
					lastUser = m.Text()
				}
			}
			content, usage := "ok", map[string]int{"prompt_tokens": 10, "completion_tokens": 2, "total_tokens": 12}
			if strings.Contains(lastUser, "Return only a JSON array") {
				content = reply.Load().(string)
				usage = map[string]int{"prompt_tokens": 30, "completion_tokens": 4, "total_tokens": 34}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "x", "model": req.Model,
				"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"}},
				"usage":   usage})
		default:
			http.NotFound(w, r)
		}
	}))
}

const fruitDocument = "# Apple\n\nApples are red and crunchy. They grow on trees in temperate climates and store well.\n\n# Banana\n\nBananas are yellow and soft. They grow in bunches in tropical regions and ripen fast.\n\n# Cherry\n\nCherries are small and sweet. They are harvested in early summer and eaten fresh."

// rerankProject builds a store with rerank on and a project that uses it,
// and returns the project's API key.
func rerankProject(t *testing.T, e *env, connID int64, store map[string]any) string {
	t.Helper()
	return rerankProjectWith(t, e, connID, connID, store)
}

// rerankProjectWith is rerankProject with the project's chat connection apart
// from the store's embedding connection, as a real deployment has them.
func rerankProjectWith(t *testing.T, e *env, embedID, chatID int64, store map[string]any) string {
	t.Helper()
	connID := embedID
	store["name"], store["embedding_connection_id"] = "fruit", connID
	store["chunk_size"], store["chunk_overlap"], store["top_k"], store["rerank"] = 200, 0, 2, true
	rs := e.call("POST", "/admin/api/rag-stores", store, "")
	if rs["_status"] != float64(201) {
		t.Fatalf("create store: %v", rs)
	}
	storeID := int64(rs["id"].(float64))
	doc := e.upload(storeID, "fruits.md", fruitDocument)
	e.waitReady(int64(doc["id"].(float64)))
	proj := e.call("POST", "/admin/api/projects", map[string]any{"name": "app", "model_connection_id": chatID, "rag_store_id": storeID}, "")
	if proj["_status"] != float64(201) {
		t.Fatalf("create project: %v", proj)
	}
	return proj["api_key"].(string)
}

// callForRow sends one /v1 request and returns its request_logs row.
func callForRow(t *testing.T, e *env, path string, body map[string]any, key string) embedLog {
	t.Helper()
	res, hdr := e.callRaw("POST", path, body, key)
	if res["_status"] != float64(200) {
		t.Fatalf("%s: %v", path, res)
	}
	row, found := e.logRow(hdr.Get("X-Request-Id"))
	if !found {
		t.Fatalf("no request_logs row for %s", path)
	}
	return row
}

// chatWithRerank builds a store with rerank on, a project that uses it and
// sends one RAG chat request. It returns the request's log row.
func chatWithRerank(t *testing.T, e *env, connID int64, store map[string]any) embedLog {
	t.Helper()
	key := rerankProject(t, e, connID, store)
	return callForRow(t, e, "/v1/chat/completions", map[string]any{"model": "whatever",
		"messages": []map[string]string{{"role": "user", "content": "tell me about fruit"}}}, key)
}

// The rerank of a RAG chat request is a side call: with the llm backend its
// chat tokens (30+4) are added to the request's own usage (10+2) on the
// request's log row.
func TestChatCountsLLMRerankTokens(t *testing.T) {
	var reply atomic.Value
	reply.Store("[2, 1]")
	up := rankingUpstream(&reply)
	defer up.Close()
	e := newEnv(t, testdb.Config(t))
	conn := e.call("POST", "/admin/api/models", map[string]any{"name": "mock", "provider_type": "custom_openai",
		"base_url": up.URL + "/v1", "api_key": "k", "model_name": "mock-model"}, "")

	row := chatWithRerank(t, e, int64(conn["id"].(float64)), map[string]any{})
	if row.promptTokens != 40 || row.completionTokens != 6 || row.estimated {
		t.Errorf("row tokens: %+v, want 40 prompt (10+30) and 6 completion (2+4), not estimated", row)
	}
}

// With an API backend the reranker's own count (42, from the upstream) is
// added to the request's usage.
func TestChatCountsAPIRerankTokens(t *testing.T) {
	var reply atomic.Value
	reply.Store("[2, 1]")
	up := rankingUpstream(&reply)
	defer up.Close()
	var calls atomic.Int64
	rr := rerankUpstream(&calls, nil)
	defer rr.Close()
	e := newEnv(t, testdb.Config(t))
	conn := e.call("POST", "/admin/api/models", map[string]any{"name": "mock", "provider_type": "custom_openai",
		"base_url": up.URL + "/v1", "api_key": "k", "model_name": "mock-model"}, "")
	co := e.call("POST", "/admin/api/models", map[string]any{"name": "cohere", "provider_type": "cohere_rerank",
		"base_url": rr.URL, "api_key": "sk-co", "model_name": "rerank-v3.5"}, "")
	if co["_status"] != float64(201) {
		t.Fatalf("rerank connection: %v", co)
	}

	row := chatWithRerank(t, e, int64(conn["id"].(float64)), map[string]any{"rerank_backend": "cohere",
		"rerank_connection_id": int64(co["id"].(float64))})
	if calls.Load() != 1 {
		t.Fatalf("rerank upstream calls: %d, want 1", calls.Load())
	}
	if row.promptTokens != 52 || row.completionTokens != 2 || row.estimated {
		t.Errorf("row tokens: %+v, want 52 prompt (10+42) and 2 completion, not estimated", row)
	}
}

// A chat model lists only the passages it finds relevant. /v1/rerank returns
// the rest after them with a score of 0, and an empty list ("none is
// relevant") scores every document 0 instead of passing the client's order
// off as a ranking.
func TestRerankChatModelScoresOnlyRankedDocuments(t *testing.T) {
	var reply atomic.Value
	up := rankingUpstream(&reply)
	defer up.Close()
	e := newEnv(t, testdb.Config(t))
	conn := e.call("POST", "/admin/api/models", map[string]any{"name": "chat", "provider_type": "custom_openai",
		"base_url": up.URL + "/v1", "api_key": "k", "model_name": "rank-model"}, "")
	chatID := int64(conn["id"].(float64))
	rs := e.call("POST", "/admin/api/rag-stores", map[string]any{"name": "s", "embedding_connection_id": chatID,
		"chunk_size": 200, "chunk_overlap": 0, "top_k": 2, "rerank": true, "rerank_connection_id": chatID}, "")
	if rs["_status"] != float64(201) {
		t.Fatalf("create store: %v", rs)
	}
	proj := e.call("POST", "/admin/api/projects", map[string]any{"name": "app", "model_connection_id": chatID,
		"rag_store_id": int64(rs["id"].(float64))}, "")
	pid := int64(proj["project"].(map[string]any)["id"].(float64))
	k := e.call("POST", "/admin/api/keys", map[string]any{"kind": "gateway", "name": "rr", "scopes": []string{"rerank"},
		"project_ids": []int64{pid}, "default_project_id": pid}, "")
	docs := []string{"first", "second", "third", "fourth"}

	type result struct {
		index int
		score float64
	}
	rerank := func(t *testing.T, content string) []result {
		t.Helper()
		reply.Store(content)
		res := e.call("POST", "/v1/rerank", map[string]any{"query": "which is best", "documents": docs}, k["api_key"].(string))
		if res["_status"] != float64(200) {
			t.Fatalf("rerank: %v", res)
		}
		var out []result
		for _, it := range res["results"].([]any) {
			m := it.(map[string]any)
			out = append(out, result{int(m["index"].(float64)), m["relevance_score"].(float64)})
		}
		return out
	}

	t.Run("partial ranking", func(t *testing.T) {
		got := rerank(t, "[3, 2]")
		want := []result{{2, 1}, {1, 0.5}, {0, 0}, {3, 0}}
		if len(got) != len(want) {
			t.Fatalf("results: %v", got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("result %d: %v, want %v (all: %v)", i, got[i], want[i], got)
			}
		}
	})
	t.Run("empty ranking", func(t *testing.T) {
		got := rerank(t, "[]")
		if len(got) != len(docs) {
			t.Fatalf("results: %v", got)
		}
		for i, r := range got {
			if r.score != 0 || r.index != i {
				t.Errorf("result %d: %v, want index %d and score 0", i, r, i)
			}
		}
	})
	t.Run("full ranking", func(t *testing.T) {
		got := rerank(t, "[4, 3, 2, 1]")
		for i, r := range got {
			if r.index != 3-i || r.score != float64(4-i)/4 {
				t.Errorf("result %d: %v", i, r)
			}
		}
	})
}

// The chat model's price covers the chat model's tokens only. An LLM rerank
// is priced with it (the rerank ran on that model), while the tokens of an API
// reranker whose connection has no price row are counted on the row but add
// nothing to the cost. (With a price row they are priced; see the tests below.)
func TestChatPricesLLMRerankButNotAPIRerankTokens(t *testing.T) {
	costOf := func(t *testing.T, backend, endpoint string) (embedLog, int64) {
		t.Helper()
		var reply atomic.Value
		reply.Store("[2, 1]")
		up := rankingUpstream(&reply)
		defer up.Close()
		var calls atomic.Int64
		rr := rerankUpstream(&calls, nil)
		defer rr.Close()
		var cache *pricing.Cache
		e := newEnvOpts(t, testdb.Config(t), envOpts{bootstrap: true, gwTune: func(g *gateway.Gateway) {
			cache = pricing.NewCache(g.Store.DB(), slog.New(slog.NewTextHandler(io.Discard, nil)))
			g.Prices = cache
		}})
		// 1 USD per million input tokens is one micro-dollar per token.
		price := e.call("POST", "/admin/api/prices", map[string]any{"provider_type": "custom_openai",
			"model_pattern": "mock-model", "input_per_mtok": 1, "output_per_mtok": 2, "currency": "usd"}, "")
		if price["_status"] != float64(201) {
			t.Fatalf("create price: %v", price)
		}
		cache.Invalidate()
		conn := e.call("POST", "/admin/api/models", map[string]any{"name": "mock", "provider_type": "custom_openai",
			"base_url": up.URL + "/v1", "api_key": "k", "model_name": "mock-model"}, "")
		store := map[string]any{}
		if backend == "cohere" {
			co := e.call("POST", "/admin/api/models", map[string]any{"name": "cohere", "provider_type": "cohere_rerank",
				"base_url": rr.URL, "api_key": "sk-co", "model_name": "rerank-v3.5"}, "")
			store["rerank_backend"], store["rerank_connection_id"] = "cohere", int64(co["id"].(float64))
		}
		var row embedLog
		if endpoint == "retrieve" {
			key := rerankProject(t, e, int64(conn["id"].(float64)), store)
			row = callForRow(t, e, "/v1/retrieve", map[string]any{"query": "tell me about fruit"}, key)
		} else {
			row = chatWithRerank(t, e, int64(conn["id"].(float64)), store)
		}
		var micros int64
		if err := e.store.DB().QueryRow(context.Background(),
			`SELECT cost_micros FROM request_logs WHERE endpoint = $1 ORDER BY id DESC LIMIT 1`, row.endpoint).Scan(&micros); err != nil {
			t.Fatalf("read cost: %v", err)
		}
		return row, micros
	}

	row, micros := costOf(t, "llm", "chat")
	// 40 prompt x 1 + 6 completion x 2: the rerank's tokens are the chat model's.
	if row.promptTokens != 40 || micros != 52 {
		t.Errorf("llm rerank: prompt %d, cost %d micros, want 40 and 52", row.promptTokens, micros)
	}
	row, micros = costOf(t, "cohere", "chat")
	// 52 prompt tokens on the row, but only the chat call's 10 x 1 + 2 x 2 are priced.
	if row.promptTokens != 52 || micros != 14 {
		t.Errorf("api rerank: prompt %d, cost %d micros, want 52 and 14", row.promptTokens, micros)
	}

	// /v1/retrieve fills the unpriced share on its own path: the row is the
	// embedding model's, and the reranker's 42 tokens are not priced with it.
	row, micros = costOf(t, "cohere", "retrieve")
	if row.completionTokens != 0 || row.promptTokens <= 42 || micros != int64(row.promptTokens-42) {
		t.Errorf("api rerank in retrieve: prompt %d, completion %d, cost %d micros, want cost = prompt - 42 at 1 micro per token",
			row.promptTokens, row.completionTokens, micros)
	}
}

// newPricedEnv is an env whose gateway prices requests from the price table.
// The table starts empty (the harness does not seed it), so every test states
// exactly the prices it relies on.
func newPricedEnv(t *testing.T) (*env, *pricing.Cache) {
	t.Helper()
	var cache *pricing.Cache
	e := newEnvOpts(t, testdb.Config(t), envOpts{bootstrap: true, gwTune: func(g *gateway.Gateway) {
		cache = pricing.NewCache(g.Store.DB(), slog.New(slog.NewTextHandler(io.Discard, nil)))
		g.Prices = cache
		g.MaxRetries, g.UpstreamTimeout = 2, 10*time.Second
	}})
	return e, cache
}

// setPrice creates one price row through the admin API. perUnit is the
// per_search_unit price, nil for a token-priced model.
func setPrice(t *testing.T, e *env, cache *pricing.Cache, providerType, model string, in, out float64, perUnit *float64) {
	t.Helper()
	body := map[string]any{"provider_type": providerType, "model_pattern": model,
		"input_per_mtok": in, "output_per_mtok": out, "currency": "usd"}
	if perUnit != nil {
		body["per_search_unit"] = *perUnit
	}
	res := e.call("POST", "/admin/api/prices", body, "")
	if res["_status"] != float64(201) {
		t.Fatalf("create price %s/%s: %v", providerType, model, res)
	}
	cache.Invalidate()
}

func connection(t *testing.T, e *env, name, providerType, baseURL, model string) int64 {
	t.Helper()
	c := e.call("POST", "/admin/api/models", map[string]any{"name": name, "provider_type": providerType,
		"base_url": baseURL, "api_key": "k", "model_name": model}, "")
	if c["_status"] != float64(201) {
		t.Fatalf("create connection %s: %v", name, c)
	}
	return int64(c["id"].(float64))
}

// pricedCall sends one /v1 request and returns its row with the cost the
// gateway wrote.
func pricedCall(t *testing.T, e *env, path string, body map[string]any, key string) (row embedLog, micros int64, source string) {
	t.Helper()
	res, hdr := e.callRaw("POST", path, body, key)
	if res["_status"] != float64(200) {
		t.Fatalf("%s: %v", path, res)
	}
	id := hdr.Get("X-Request-Id")
	row, found := e.logRow(id)
	if !found {
		t.Fatalf("no request_logs row for %s", path)
	}
	if err := e.store.DB().QueryRow(context.Background(),
		`SELECT cost_micros, cost_source FROM request_logs WHERE request_id = $1`, id).Scan(&micros, &source); err != nil {
		t.Fatalf("read cost: %v", err)
	}
	return row, micros, source
}

// cohereUpstream answers /v2/rerank like Cohere: 42 input tokens and the
// given number of billed search units, or no billed_units when units is 0.
func cohereUpstream(units int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/rerank" {
			http.NotFound(w, r)
			return
		}
		var req struct {
			Documents []string `json:"documents"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		results := make([]map[string]any, len(req.Documents))
		for i := range req.Documents {
			results[i] = map[string]any{"index": i, "relevance_score": 1 - float64(i)/10}
		}
		meta := map[string]any{"tokens": map[string]int{"input_tokens": 42, "output_tokens": 0}}
		if units > 0 {
			meta["billed_units"] = map[string]int{"search_units": units}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "r", "results": results, "meta": meta})
	}))
}

// voyageUpstream answers /v1/rerank like Voyage, reporting total_tokens.
func voyageUpstream(tokens int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/rerank" {
			http.NotFound(w, r)
			return
		}
		var req struct {
			Documents []string `json:"documents"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		data := make([]map[string]any, len(req.Documents))
		for i := range req.Documents {
			data[i] = map[string]any{"index": i, "relevance_score": 1 - float64(i)/10}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data, "usage": map[string]int{"total_tokens": tokens}})
	}))
}

func ptr(v float64) *float64 { return &v }

// A RAG chat request reranked by Cohere costs the chat call plus what Cohere
// bills for the rerank, search units times the price of one. The reranker's
// 42 tokens are counted on the row but are not what Cohere charges for.
func TestChatPricesCohereRerankBySearchUnit(t *testing.T) {
	var reply atomic.Value
	reply.Store("[2, 1]")
	up := rankingUpstream(&reply)
	defer up.Close()
	rr := cohereUpstream(3)
	defer rr.Close()
	e, cache := newPricedEnv(t)
	// 1 USD per million tokens is one micro-dollar per token.
	setPrice(t, e, cache, "custom_openai", "mock-model", 1, 2, nil)
	// 0.001 USD per search unit is 1000 micros per unit.
	setPrice(t, e, cache, "cohere_rerank", "rerank-v3.5", 0, 0, ptr(0.001))
	chat := connection(t, e, "mock", "custom_openai", up.URL+"/v1", "mock-model")
	co := connection(t, e, "cohere", "cohere_rerank", rr.URL, "rerank-v3.5")

	key := rerankProject(t, e, chat, map[string]any{"rerank_backend": "cohere", "rerank_connection_id": co})
	row, micros, source := pricedCall(t, e, "/v1/chat/completions", map[string]any{"model": "whatever",
		"messages": []map[string]string{{"role": "user", "content": "tell me about fruit"}}}, key)
	if row.promptTokens != 52 || row.completionTokens != 2 {
		t.Errorf("row tokens: %+v, want 52 prompt (10+42) and 2 completion: the token counts must not change", row)
	}
	// 10 x 1 + 2 x 2 for the chat call, 3 search units x 1000 for the rerank.
	if micros != 3014 {
		t.Errorf("cost %d micros, want 3014 (14 chat + 3 x 1000 search units)", micros)
	}
	if source != "user" {
		t.Errorf("cost_source = %q, want the main call's (user)", source)
	}
}

// A Cohere response that bills no search units is one search unit.
func TestChatPricesCohereRerankDefaultsToOneSearchUnit(t *testing.T) {
	var reply atomic.Value
	reply.Store("[2, 1]")
	up := rankingUpstream(&reply)
	defer up.Close()
	rr := cohereUpstream(0)
	defer rr.Close()
	e, cache := newPricedEnv(t)
	setPrice(t, e, cache, "custom_openai", "mock-model", 1, 2, nil)
	setPrice(t, e, cache, "cohere_rerank", "rerank-v3.5", 0, 0, ptr(0.001))
	chat := connection(t, e, "mock", "custom_openai", up.URL+"/v1", "mock-model")
	co := connection(t, e, "cohere", "cohere_rerank", rr.URL, "rerank-v3.5")

	key := rerankProject(t, e, chat, map[string]any{"rerank_backend": "cohere", "rerank_connection_id": co})
	_, micros, _ := pricedCall(t, e, "/v1/chat/completions", map[string]any{"model": "whatever",
		"messages": []map[string]string{{"role": "user", "content": "tell me about fruit"}}}, key)
	if micros != 1014 {
		t.Errorf("cost %d micros, want 1014 (14 chat + 1 default search unit x 1000)", micros)
	}
}

// /v1/retrieve with a Voyage reranker: the row's own model is the embedding
// model, and the 100 tokens Voyage reports are priced at the Voyage price.
func TestRetrievePricesVoyageRerankTokens(t *testing.T) {
	var reply atomic.Value
	up := rankingUpstream(&reply)
	defer up.Close()
	rr := voyageUpstream(100)
	defer rr.Close()
	e, cache := newPricedEnv(t)
	setPrice(t, e, cache, "custom_openai", "embed-model", 1, 0, nil)
	setPrice(t, e, cache, "voyage_rerank", "rerank-2.5", 4, 0, nil)
	emb := connection(t, e, "embed", "custom_openai", up.URL+"/v1", "embed-model")
	vo := connection(t, e, "voyage", "voyage_rerank", rr.URL, "rerank-2.5")

	key := rerankProject(t, e, emb, map[string]any{"rerank_backend": "voyage", "rerank_connection_id": vo})
	row, micros, _ := pricedCall(t, e, "/v1/retrieve", map[string]any{"query": "tell me about fruit"}, key)
	if row.promptTokens <= 100 {
		t.Fatalf("row prompt tokens %d, want the query's embedding tokens plus Voyage's 100", row.promptTokens)
	}
	// The embedding tokens at 1 micro each, Voyage's 100 at 4 micros each.
	if want := int64(row.promptTokens-100) + 400; micros != want {
		t.Errorf("cost %d micros, want %d (embedding at 1, 100 rerank tokens at 4)", micros, want)
	}
}

// /v1/retrieve with an llm rerank: the project's chat model did the rerank, so
// its 30+4 tokens are priced at the chat connection's price and not at the embedding
// connection's, whose row they are written on.
func TestRetrievePricesLLMRerankAtTheChatConnection(t *testing.T) {
	var reply atomic.Value
	reply.Store("[2, 1]")
	up := rankingUpstream(&reply)
	defer up.Close()
	e, cache := newPricedEnv(t)
	setPrice(t, e, cache, "custom_openai", "embed-model", 1, 0, nil)
	setPrice(t, e, cache, "custom_openai", "chat-model", 3, 5, nil)
	emb := connection(t, e, "embed", "custom_openai", up.URL+"/v1", "embed-model")
	chat := connection(t, e, "chat", "custom_openai", up.URL+"/v1", "chat-model")

	key := rerankProjectWith(t, e, emb, chat, map[string]any{})
	row, micros, _ := pricedCall(t, e, "/v1/retrieve", map[string]any{"query": "tell me about fruit"}, key)
	if row.promptTokens <= 30 || row.completionTokens != 4 {
		t.Fatalf("row tokens: %+v, want the embedding tokens + 30 prompt and 4 completion", row)
	}
	// Embedding tokens at 1, the rerank's 30 at 3 and its 4 completion tokens at 5.
	if want := int64(row.promptTokens-30) + 30*3 + 4*5; micros != want {
		t.Errorf("cost %d micros, want %d (rerank priced at the chat connection, not the embedding one)", micros, want)
	}
}

// failingChatUpstream answers a rerank prompt (30+4 tokens) and fails every
// other chat request with a 500, the way a target that goes down right after
// the rerank does.
func failingChatUpstream() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/v1/chat/completions" || !strings.Contains(string(body), "Return only a JSON array") {
			http.Error(w, `{"error":{"message":"down","type":"server_error"}}`, http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "x", "model": "m",
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "[2, 1]"}, "finish_reason": "stop"}},
			"usage":   map[string]int{"prompt_tokens": 30, "completion_tokens": 4, "total_tokens": 34}})
	}))
}

// In a routed chat the llm rerank runs through the first target. When that
// target then fails and the second answers, the rerank's tokens are still
// priced at the first target's model, which did the rerank, and the answer at
// the second's.
func TestRoutedChatPricesRerankAtTheTargetThatRanOne(t *testing.T) {
	var reply atomic.Value
	reply.Store("[2, 1]")
	upA := failingChatUpstream()
	defer upA.Close()
	upB := rankingUpstream(&reply)
	defer upB.Close()
	e, cache := newPricedEnv(t)
	setPrice(t, e, cache, "custom_openai", "model-a", 7, 9, nil)
	setPrice(t, e, cache, "custom_openai", "model-b", 1, 2, nil)
	connA := connection(t, e, "a", "custom_openai", upA.URL+"/v1", "model-a")
	connB := connection(t, e, "b", "custom_openai", upB.URL+"/v1", "model-b")

	key := rerankProject(t, e, connB, map[string]any{})
	var pid int64
	if err := e.store.DB().QueryRow(context.Background(), `SELECT id FROM projects LIMIT 1`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	route := e.call("POST", fmt.Sprintf("/admin/api/projects/%d/routes", pid), map[string]any{"name": "smart", "targets": []any{
		map[string]any{"connection_id": connA, "priority": 0},
		map[string]any{"connection_id": connB, "priority": 1}}}, "")
	if route["_status"] != float64(201) {
		t.Fatalf("create route: %v", route)
	}

	row, micros, _ := pricedCall(t, e, "/v1/chat/completions", map[string]any{"model": "smart",
		"messages": []map[string]string{{"role": "user", "content": "tell me about fruit"}}}, key)
	if row.promptTokens != 40 || row.completionTokens != 6 || row.modelName != "model-b" {
		t.Fatalf("row: %+v, want 40 prompt, 6 completion, answered by model-b", row)
	}
	// Rerank at model-a: 30 x 7 + 4 x 9 = 246. Answer at model-b: 10 x 1 + 2 x 2 = 14.
	if micros != 260 {
		t.Errorf("cost %d micros, want 260 (246 rerank at the first target + 14 answer at the fallback)", micros)
	}
}

// A rerank connection nobody priced adds nothing to the cost, and the request
// still succeeds with the main call's cost and cost_source.
func TestUnpricedRerankConnectionAddsNothing(t *testing.T) {
	var reply atomic.Value
	reply.Store("[2, 1]")
	up := rankingUpstream(&reply)
	defer up.Close()
	rr := cohereUpstream(3)
	defer rr.Close()
	e, cache := newPricedEnv(t)
	setPrice(t, e, cache, "custom_openai", "mock-model", 1, 2, nil)
	chat := connection(t, e, "mock", "custom_openai", up.URL+"/v1", "mock-model")
	co := connection(t, e, "cohere", "cohere_rerank", rr.URL, "rerank-v3.5")

	key := rerankProject(t, e, chat, map[string]any{"rerank_backend": "cohere", "rerank_connection_id": co})
	row, micros, source := pricedCall(t, e, "/v1/chat/completions", map[string]any{"model": "whatever",
		"messages": []map[string]string{{"role": "user", "content": "tell me about fruit"}}}, key)
	if row.promptTokens != 52 {
		t.Errorf("row prompt tokens %d, want 52: the tokens are still counted", row.promptTokens)
	}
	if micros != 14 || source != "user" {
		t.Errorf("cost %d micros from %q, want 14 from user (the chat call alone)", micros, source)
	}
}

// A /v1/rerank row is the rerank connection's own: its 42 tokens are counted
// and its 2 search units are priced with its per_search_unit.
func TestRerankEndpointPricesItsOwnConnection(t *testing.T) {
	var reply atomic.Value
	up := rankingUpstream(&reply)
	defer up.Close()
	rr := cohereUpstream(2)
	defer rr.Close()
	e, cache := newPricedEnv(t)
	setPrice(t, e, cache, "cohere_rerank", "rerank-v3.5", 0, 0, ptr(0.001))
	chat := connection(t, e, "chat", "custom_openai", up.URL+"/v1", "chat-model")
	co := connection(t, e, "cohere", "cohere_rerank", rr.URL, "rerank-v3.5")
	rs := e.call("POST", "/admin/api/rag-stores", map[string]any{"name": "s", "embedding_connection_id": chat,
		"chunk_size": 200, "chunk_overlap": 0, "top_k": 2, "rerank": true,
		"rerank_backend": "cohere", "rerank_connection_id": co}, "")
	if rs["_status"] != float64(201) {
		t.Fatalf("create store: %v", rs)
	}
	proj := e.call("POST", "/admin/api/projects", map[string]any{"name": "app", "model_connection_id": chat,
		"rag_store_id": int64(rs["id"].(float64))}, "")
	pid := int64(proj["project"].(map[string]any)["id"].(float64))
	k := e.call("POST", "/admin/api/keys", map[string]any{"kind": "gateway", "name": "rr", "scopes": []string{"rerank"},
		"project_ids": []int64{pid}, "default_project_id": pid}, "")

	row, micros, source := pricedCall(t, e, "/v1/rerank", map[string]any{"model": "rerank-v3.5", "query": "which is best",
		"documents": []string{"first", "second", "third"}}, k["api_key"].(string))
	if row.endpoint != "rerank" || row.promptTokens != 42 {
		t.Errorf("row: %+v, want endpoint rerank and 42 prompt tokens", row)
	}
	if micros != 2000 || source != "user" {
		t.Errorf("cost %d micros from %q, want 2000 (2 search units x 1000) from user", micros, source)
	}
}

// An admin who types token prices into a Cohere row must not be charged twice:
// a row with a per_search_unit rate prices rerank by search units alone, both
// inside a chat request and on /v1/rerank. Cohere reports 42 input tokens
// here, which would add 42 x 5 = 210 micros if the token price were used.
func TestCohereRowWithTokenPricesStillPricesBySearchUnitOnly(t *testing.T) {
	var reply atomic.Value
	reply.Store("[2, 1]")
	up := rankingUpstream(&reply)
	defer up.Close()
	rr := cohereUpstream(2)
	defer rr.Close()
	e, cache := newPricedEnv(t)
	setPrice(t, e, cache, "custom_openai", "mock-model", 1, 2, nil)
	// Token prices and a search unit rate on the same Cohere row.
	setPrice(t, e, cache, "cohere_rerank", "rerank-v3.5", 5, 7, ptr(0.001))
	chat := connection(t, e, "mock", "custom_openai", up.URL+"/v1", "mock-model")
	co := connection(t, e, "cohere", "cohere_rerank", rr.URL, "rerank-v3.5")

	key := rerankProject(t, e, chat, map[string]any{"rerank_backend": "cohere", "rerank_connection_id": co})
	row, micros, _ := pricedCall(t, e, "/v1/chat/completions", map[string]any{"model": "whatever",
		"messages": []map[string]string{{"role": "user", "content": "tell me about fruit"}}}, key)
	if row.promptTokens != 52 {
		t.Errorf("row prompt tokens = %d, want 52: the token counts must not change", row.promptTokens)
	}
	// 14 for the chat call, 2 search units x 1000; no 42 x 5 on top.
	if micros != 2014 {
		t.Errorf("chat cost %d micros, want 2014 (14 chat + 2 x 1000 search units, no token price)", micros)
	}

	rs := e.call("POST", "/admin/api/rag-stores", map[string]any{"name": "s", "embedding_connection_id": chat,
		"chunk_size": 200, "chunk_overlap": 0, "top_k": 2, "rerank": true,
		"rerank_backend": "cohere", "rerank_connection_id": co}, "")
	if rs["_status"] != float64(201) {
		t.Fatalf("create store: %v", rs)
	}
	proj := e.call("POST", "/admin/api/projects", map[string]any{"name": "app2", "model_connection_id": chat,
		"rag_store_id": int64(rs["id"].(float64))}, "")
	pid := int64(proj["project"].(map[string]any)["id"].(float64))
	k := e.call("POST", "/admin/api/keys", map[string]any{"kind": "gateway", "name": "rr", "scopes": []string{"rerank"},
		"project_ids": []int64{pid}, "default_project_id": pid}, "")
	rrow, rmicros, _ := pricedCall(t, e, "/v1/rerank", map[string]any{"model": "rerank-v3.5", "query": "which is best",
		"documents": []string{"first", "second", "third"}}, k["api_key"].(string))
	if rrow.endpoint != "rerank" || rrow.promptTokens != 42 {
		t.Errorf("rerank row: %+v, want endpoint rerank and 42 prompt tokens", rrow)
	}
	if rmicros != 2000 {
		t.Errorf("/v1/rerank cost %d micros, want 2000 (2 search units x 1000, no token price)", rmicros)
	}
}

// A per_search_unit that is set to an explicit 0 is still a search-unit row
// (ADR-021: only a NULL rate means the row bills by tokens): rerank on it is free
// even though the row carries token prices, in a chat and on /v1/rerank.
func TestCohereRowWithZeroSearchUnitRateIsNotBilledByTokens(t *testing.T) {
	var reply atomic.Value
	reply.Store("[2, 1]")
	up := rankingUpstream(&reply)
	defer up.Close()
	rr := cohereUpstream(2)
	defer rr.Close()
	e, cache := newPricedEnv(t)
	setPrice(t, e, cache, "custom_openai", "mock-model", 1, 2, nil)
	setPrice(t, e, cache, "cohere_rerank", "rerank-v3.5", 5, 7, ptr(0))
	chat := connection(t, e, "mock", "custom_openai", up.URL+"/v1", "mock-model")
	co := connection(t, e, "cohere", "cohere_rerank", rr.URL, "rerank-v3.5")

	key := rerankProject(t, e, chat, map[string]any{"rerank_backend": "cohere", "rerank_connection_id": co})
	_, micros, _ := pricedCall(t, e, "/v1/chat/completions", map[string]any{"model": "whatever",
		"messages": []map[string]string{{"role": "user", "content": "tell me about fruit"}}}, key)
	if micros != 14 {
		t.Errorf("chat cost %d micros, want 14 (the chat call; rerank at a zero search unit rate is free)", micros)
	}

	rs := e.call("POST", "/admin/api/rag-stores", map[string]any{"name": "s", "embedding_connection_id": chat,
		"chunk_size": 200, "chunk_overlap": 0, "top_k": 2, "rerank": true,
		"rerank_backend": "cohere", "rerank_connection_id": co}, "")
	if rs["_status"] != float64(201) {
		t.Fatalf("create store: %v", rs)
	}
	proj := e.call("POST", "/admin/api/projects", map[string]any{"name": "app3", "model_connection_id": chat,
		"rag_store_id": int64(rs["id"].(float64))}, "")
	pid := int64(proj["project"].(map[string]any)["id"].(float64))
	k := e.call("POST", "/admin/api/keys", map[string]any{"kind": "gateway", "name": "rr", "scopes": []string{"rerank"},
		"project_ids": []int64{pid}, "default_project_id": pid}, "")
	_, rmicros, _ := pricedCall(t, e, "/v1/rerank", map[string]any{"model": "rerank-v3.5", "query": "which is best",
		"documents": []string{"first", "second", "third"}}, k["api_key"].(string))
	if rmicros != 0 {
		t.Errorf("/v1/rerank cost %d micros, want 0 (zero search unit rate, no token price)", rmicros)
	}
}
