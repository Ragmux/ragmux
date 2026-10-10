package e2e

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

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
	store["name"], store["embedding_connection_id"] = "fruit", connID
	store["chunk_size"], store["chunk_overlap"], store["top_k"], store["rerank"] = 200, 0, 2, true
	rs := e.call("POST", "/admin/api/rag-stores", store, "")
	if rs["_status"] != float64(201) {
		t.Fatalf("create store: %v", rs)
	}
	storeID := int64(rs["id"].(float64))
	doc := e.upload(storeID, "fruits.md", fruitDocument)
	e.waitReady(int64(doc["id"].(float64)))
	proj := e.call("POST", "/admin/api/projects", map[string]any{"name": "app", "model_connection_id": connID, "rag_store_id": storeID}, "")
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
// is priced with it (the rerank ran on that model), while an API reranker's
// tokens are counted on the row but add nothing to the cost: they are not
// the row's model's tokens, and the API bills by search unit.
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
