package e2e

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ragmux/ragmux/internal/testdb"
)

// embeddingUpstream is an OpenAI-compatible embeddings server that reports
// usage (5 prompt tokens per input) and counts the calls it receives. It
// records the model of the last call so a test can tell which route served it.
func embeddingUpstream(calls *atomic.Int64, lastModel *atomic.Value) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Input []string `json:"input"`
			Model string   `json:"model"`
		}
		_ = json.Unmarshal(body, &req)
		if lastModel != nil {
			lastModel.Store(req.Model)
		}
		data := make([]map[string]any, len(req.Input))
		for i, in := range req.Input {
			data[i] = map[string]any{"object": "embedding", "index": i, "embedding": keywordVec(in)}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data, "model": req.Model,
			"usage": map[string]int{"prompt_tokens": 5 * len(req.Input), "total_tokens": 5 * len(req.Input)}})
	}))
}

// embedLog is the part of a request_logs row the embeddings and retrieve
// tests assert on.
type embedLog struct {
	endpoint, requestedModel, route, modelName string
	status, promptTokens, completionTokens     int
	estimated                                  bool
}

// logRow reads the request_logs row stamped with id; the row is written after
// the response is sent, so it polls briefly.
func (e *env) logRow(id string) (embedLog, bool) {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var l embedLog
		var ep, rm, rt, mn *string
		err := e.store.DB().QueryRow(context.Background(),
			`SELECT endpoint, requested_model, route, model_name, status_code, prompt_tokens, completion_tokens, estimated
			 FROM request_logs WHERE request_id = $1`, id).
			Scan(&ep, &rm, &rt, &mn, &l.status, &l.promptTokens, &l.completionTokens, &l.estimated)
		if err == nil {
			for dst, src := range map[*string]*string{&l.endpoint: ep, &l.requestedModel: rm, &l.route: rt, &l.modelName: mn} {
				if src != nil {
					*dst = *src
				}
			}
			return l, true
		}
		if time.Now().After(deadline) {
			return embedLog{}, false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestEmbeddingsEndpoint(t *testing.T) {
	var embCalls, chatCalls atomic.Int64
	var lastModel atomic.Value
	embUp := embeddingUpstream(&embCalls, &lastModel)
	defer embUp.Close()
	chatUp := countingUpstream(&chatCalls)
	defer chatUp.Close()
	// plain reports no usage, so its tokens are estimated.
	plainUp := mockUpstream(t)
	defer plainUp.Close()
	e := newEnv(t, testdb.Config(t))

	mkConn := func(name, url, model string) int64 {
		c := e.call("POST", "/admin/api/models", map[string]any{"name": name, "provider_type": "custom_openai",
			"base_url": url + "/v1", "api_key": "k", "model_name": model}, "")
		if c["_status"] != float64(201) {
			t.Fatalf("create connection %s: %v", name, c)
		}
		return int64(c["id"].(float64))
	}
	embConn := mkConn("embed", embUp.URL, "embed-model")
	chatConn := mkConn("chat", chatUp.URL, "chat-model")
	plainConn := mkConn("plain", plainUp.URL, "plain-model")

	rs := e.call("POST", "/admin/api/rag-stores", map[string]any{"name": "s", "embedding_connection_id": embConn,
		"chunk_size": 200, "chunk_overlap": 0, "top_k": 2}, "")
	if rs["_status"] != float64(201) {
		t.Fatalf("create store: %v", rs)
	}
	proj := e.call("POST", "/admin/api/projects", map[string]any{"name": "app", "model_connection_id": chatConn,
		"rag_store_id": int64(rs["id"].(float64))}, "")
	if proj["_status"] != float64(201) {
		t.Fatalf("create project: %v", proj)
	}
	apiKey := proj["api_key"].(string)
	pid := int64(proj["project"].(map[string]any)["id"].(float64))

	// The body an OpenAI SDK sends.
	sdk := map[string]any{"model": "embed-model", "input": []string{"apple pie", "banana split"}, "encoding_format": "float"}
	res, hdr := e.callRaw("POST", "/v1/embeddings", sdk, apiKey)
	if res["_status"] != float64(200) {
		t.Fatalf("embeddings: %v", res)
	}
	data := res["data"].([]any)
	if res["object"] != "list" || len(data) != 2 || res["model"] != "embed-model" {
		t.Fatalf("response shape: %v", res)
	}
	first := data[0].(map[string]any)
	if first["object"] != "embedding" || first["index"] != float64(0) || len(first["embedding"].([]any)) != 4 {
		t.Errorf("first embedding: %v", first)
	}
	usage := res["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(10) || usage["total_tokens"] != float64(10) {
		t.Errorf("usage: %v", usage)
	}
	row, ok := e.logRow(hdr.Get("X-Request-Id"))
	if !ok {
		t.Fatal("no request_logs row for the embeddings call")
	}
	if row.endpoint != "embeddings" || row.promptTokens != 10 || row.completionTokens != 0 || row.estimated ||
		row.status != 200 || row.modelName != "embed-model" || row.requestedModel != "embed-model" || row.route != "" {
		t.Errorf("log row: %+v", row)
	}

	// A single string, and an empty model that means the store's connection.
	one := e.call("POST", "/v1/embeddings", map[string]any{"input": "cherry"}, apiKey)
	if one["_status"] != float64(200) || len(one["data"].([]any)) != 1 || one["model"] != "embed-model" {
		t.Errorf("string input, no model: %v", one)
	}

	// base64 carries the same float32 values, little-endian.
	b64 := e.call("POST", "/v1/embeddings", map[string]any{"input": "banana", "model": "embed-model", "encoding_format": "base64"}, apiKey)
	if b64["_status"] != float64(200) {
		t.Fatalf("base64: %v", b64)
	}
	raw, err := base64.StdEncoding.DecodeString(b64["data"].([]any)[0].(map[string]any)["embedding"].(string))
	if err != nil || len(raw) != 16 {
		t.Fatalf("base64 payload: %v (%d bytes)", err, len(raw))
	}
	want := keywordVec("banana")
	for i := range want {
		if got := math.Float32frombits(binary.LittleEndian.Uint32(raw[4*i:])); got != want[i] {
			t.Errorf("base64 value %d = %v, want %v", i, got, want[i])
		}
	}

	// Validation.
	for name, body := range map[string]map[string]any{
		"no input":        {"model": "embed-model"},
		"empty array":     {"input": []string{}},
		"empty string":    {"input": ""},
		"token ids":       {"input": []int{1, 2, 3}},
		"bad encoding":    {"input": "x", "encoding_format": "int8"},
		"dimensions":      {"input": "x", "dimensions": 8},
		"number as input": {"input": 5},
	} {
		if r := e.call("POST", "/v1/embeddings", body, apiKey); r["_status"] != float64(400) {
			t.Errorf("%s: want 400, got %v", name, r)
		}
	}

	// An unknown model is model_not_found, and never reaches any upstream, the
	// project's chat connection included.
	embBefore, chatBefore := embCalls.Load(), chatCalls.Load()
	miss := e.call("POST", "/v1/embeddings", map[string]any{"model": "text-embedding-3-small", "input": "x"}, apiKey)
	errObj, _ := miss["error"].(map[string]any)
	if miss["_status"] != float64(404) || errObj["code"] != "model_not_found" {
		t.Errorf("unknown model: %v", miss)
	}
	if embCalls.Load() != embBefore || chatCalls.Load() != chatBefore {
		t.Errorf("unknown model reached an upstream: embeddings %d->%d, chat %d->%d",
			embBefore, embCalls.Load(), chatBefore, chatCalls.Load())
	}
	// The chat connection's own model name is not an embedding model either.
	if r := e.call("POST", "/v1/embeddings", map[string]any{"model": "chat-model", "input": "x"}, apiKey); r["_status"] != float64(404) {
		t.Errorf("chat model as embedding model: %v", r)
	}

	// A kind=embeddings route answers to its name and serves its own model.
	base := fmt.Sprintf("/admin/api/projects/%d/routes", pid)
	rt := e.call("POST", base, map[string]any{"name": "alt-embed", "kind": "embeddings",
		"targets": []any{map[string]any{"connection_id": embConn, "model": "alt-model"}}}, "")
	if rt["_status"] != float64(201) {
		t.Fatalf("create embeddings route: %v", rt)
	}
	routed, rh := e.callRaw("POST", "/v1/embeddings", map[string]any{"model": "alt-embed", "input": "durian"}, apiKey)
	if routed["_status"] != float64(200) || routed["model"] != "alt-embed" || lastModel.Load() != "alt-model" {
		t.Errorf("routed call: %v upstream model %v", routed, lastModel.Load())
	}
	if row, ok := e.logRow(rh.Get("X-Request-Id")); !ok || row.route != "alt-embed" || row.modelName != "alt-model" || row.endpoint != "embeddings" {
		t.Errorf("routed log row: %+v found=%v", row, ok)
	}

	// An upstream that reports no usage gets an estimate, flagged as one.
	est := e.call("POST", base, map[string]any{"name": "no-usage", "kind": "embeddings",
		"targets": []any{map[string]any{"connection_id": plainConn}}}, "")
	if est["_status"] != float64(201) {
		t.Fatalf("create no-usage route: %v", est)
	}
	e1, eh := e.callRaw("POST", "/v1/embeddings", map[string]any{"model": "no-usage", "input": "apple orchard in spring"}, apiKey)
	if e1["_status"] != float64(200) || e1["usage"].(map[string]any)["prompt_tokens"].(float64) <= 0 {
		t.Errorf("estimated usage: %v", e1)
	}
	if row, ok := e.logRow(eh.Get("X-Request-Id")); !ok || !row.estimated || row.promptTokens <= 0 {
		t.Errorf("estimated log row: %+v found=%v", row, ok)
	}

	// A key without the embeddings scope is refused.
	chatOnly := e.call("POST", "/admin/api/keys", map[string]any{"kind": "gateway", "name": "chat-only",
		"scopes": []string{"chat"}, "project_ids": []int64{pid}, "default_project_id": pid}, "")
	if chatOnly["_status"] != float64(201) {
		t.Fatalf("create key: %v", chatOnly)
	}
	if r := e.call("POST", "/v1/embeddings", sdk, chatOnly["api_key"].(string)); r["_status"] != float64(403) {
		t.Errorf("key without the embeddings scope: %v", r)
	}
	if r := e.call("POST", "/v1/embeddings", sdk, "sk-proj-bogus"); r["_status"] != float64(401) {
		t.Errorf("bad key: %v", r)
	}
}

// A project with no RAG store has no implicit embeddings route: only a named
// route or nothing.
func TestEmbeddingsWithoutRAGStore(t *testing.T) {
	var embCalls atomic.Int64
	up := embeddingUpstream(&embCalls, nil)
	defer up.Close()
	e := newEnv(t, testdb.Config(t))
	c := e.call("POST", "/admin/api/models", map[string]any{"name": "embed", "provider_type": "custom_openai",
		"base_url": up.URL + "/v1", "api_key": "k", "model_name": "embed-model"}, "")
	proj := e.call("POST", "/admin/api/projects", map[string]any{"name": "bare", "model_connection_id": int64(c["id"].(float64))}, "")
	apiKey := proj["api_key"].(string)
	r := e.call("POST", "/v1/embeddings", map[string]any{"model": "embed-model", "input": "x"}, apiKey)
	if errObj, _ := r["error"].(map[string]any); r["_status"] != float64(404) || errObj["code"] != "model_not_found" {
		t.Errorf("no store, no route: %v", r)
	}
	if embCalls.Load() != 0 {
		t.Errorf("the default connection was used: %d calls", embCalls.Load())
	}
}
