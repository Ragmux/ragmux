package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ragmux/ragmux/internal/store"
)

// TestAnthropicCacheControlAroundRAGContext checks the body the Anthropic
// adapter sends upstream. With RAG context injected the client's system
// breakpoint is dropped (the per-query prefix could never be read back from
// the cache); without RAG -- even with a project system prompt prepended --
// it is relayed.
func TestAnthropicCacheControlAroundRAGContext(t *testing.T) {
	e := newEnv(t, nil)
	e.attachRAG()

	var sent atomic.Pointer[string]
	anth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s := string(body)
		sent.Store(&s)
		_, _ = w.Write([]byte(`{"id":"msg_1","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn",
			"usage":{"input_tokens":10,"output_tokens":1}}`))
	}))
	t.Cleanup(anth.Close)
	ctx := context.Background()
	conn, err := e.st.CreateConnection(ctx, &store.ModelConnection{Name: "claude", ProviderType: "anthropic",
		BaseURL: anth.URL, APIKey: "sk-ant-secretsecret1234", ModelName: "claude-sonnet-4-5"})
	if err != nil {
		t.Fatal(err)
	}
	e.proj.ModelConnectionID = conn.ID
	if _, err := e.st.UpdateProject(ctx, e.proj); err != nil {
		t.Fatal(err)
	}

	body := map[string]any{"messages": []map[string]any{
		{"role": "system", "content": []map[string]any{
			{"type": "text", "text": "long stable prompt", "cache_control": map[string]any{"type": "ephemeral"}},
		}},
		{"role": "user", "content": "what colour are apples?"},
	}}
	upstream := func() map[string]json.RawMessage {
		t.Helper()
		resp, out := e.chat(body)
		if resp.StatusCode != 200 {
			t.Fatalf("status %d: %v", resp.StatusCode, out)
		}
		var req map[string]json.RawMessage
		if p := sent.Load(); p == nil || json.Unmarshal([]byte(*p), &req) != nil {
			t.Fatalf("no upstream body captured")
		}
		return req
	}

	// RAG on: context reaches the system field, the marker does not.
	req := upstream()
	raw, _ := json.Marshal(req)
	if strings.Contains(string(raw), "cache_control") {
		t.Errorf("cache_control sent with RAG context: %s", req["system"])
	}
	var sys string
	if err := json.Unmarshal(req["system"], &sys); err != nil {
		t.Fatalf("system is not the plain string: %s", req["system"])
	}
	if !strings.Contains(sys, "<context>") || !strings.Contains(sys, "apples are red") || !strings.HasSuffix(sys, "long stable prompt") {
		t.Errorf("system = %q, want the context ahead of the client prompt", sys)
	}

	// RAG off, project system prompt on: the client's marker is relayed.
	e.proj.RAGStoreID = nil
	e.proj.SystemPrompt = "Answer briefly."
	if _, err := e.st.UpdateProject(ctx, e.proj); err != nil {
		t.Fatal(err)
	}
	req = upstream()
	var blocks []struct {
		Type         string          `json:"type"`
		Text         string          `json:"text"`
		CacheControl json.RawMessage `json:"cache_control"`
	}
	if err := json.Unmarshal(req["system"], &blocks); err != nil {
		t.Fatalf("system is not a block array: %s", req["system"])
	}
	if len(blocks) != 1 || string(blocks[0].CacheControl) != `{"type":"ephemeral"}` ||
		strings.Contains(blocks[0].Text, "<context>") || !strings.HasSuffix(blocks[0].Text, "long stable prompt") {
		t.Errorf("system blocks = %s", req["system"])
	}
}
