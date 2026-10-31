package e2e

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/ragmux/ragmux/internal/gateway"
	"github.com/ragmux/ragmux/internal/mcp"
	"github.com/ragmux/ragmux/internal/obs"
	"github.com/ragmux/ragmux/internal/testdb"
)

// mcpPost sends one JSON-RPC body to /mcp and decodes the answer.
func mcpPost(t *testing.T, base, body string, hdr map[string]string) (int, http.Header, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, base+"/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("/mcp answered %d with a body that is not JSON: %q", resp.StatusCode, raw)
		}
	}
	return resp.StatusCode, resp.Header, out
}

// TestMCPServer drives ADR-030 through the whole stack: the real gateway
// authenticates, limits and accounts, the real retrieval core searches.
func TestMCPServer(t *testing.T) {
	up := mockUpstream(t)
	defer up.Close()
	var gw *gateway.Gateway
	e := newEnvOpts(t, testdb.Config(t), envOpts{bootstrap: true, gwTune: func(g *gateway.Gateway) { gw = g }})
	r := chi.NewRouter()
	r.Use(obs.RequestID)
	r.Handle("/mcp", mcp.New(mcp.Config{Backend: gw, Version: "test", AllowedOrigins: []string{"https://app.example"}}))
	srv := httptest.NewServer(r)
	defer srv.Close()

	conn := e.call("POST", "/admin/api/models", map[string]any{"name": "mock", "provider_type": "custom_openai",
		"base_url": up.URL + "/v1", "api_key": "k", "model_name": "mock-model"}, "")
	connID := int64(conn["id"].(float64))
	mkStore := func(name, text, meta string) int64 {
		rs := e.call("POST", "/admin/api/rag-stores", map[string]any{"name": name, "embedding_connection_id": connID,
			"chunk_size": 200, "chunk_overlap": 0, "top_k": 2}, "")
		if rs["_status"] != float64(201) {
			t.Fatalf("create store %q: %v", name, rs)
		}
		id := int64(rs["id"].(float64))
		doc := e.uploadWithMetadata(id, "fruits.md", text, meta)
		if doc["_status"] != float64(202) {
			t.Fatalf("upload: %v", doc)
		}
		e.waitReady(int64(doc["id"].(float64)))
		return id
	}
	storeID := mkStore("Fruit Facts", "# Banana\n\nBananas are yellow and soft. They grow in bunches in tropical regions and ripen fast.", `{"lang":"en"}`)
	mkProject := func(name string, storeID int64, extra map[string]any) (int64, map[string]any) {
		body := map[string]any{"name": name, "model_connection_id": connID}
		if storeID != 0 {
			body["rag_store_id"] = storeID
		}
		for k, v := range extra {
			body[k] = v
		}
		p := e.call("POST", "/admin/api/projects", body, "")
		if p["_status"] != float64(201) {
			t.Fatalf("create project %q: %v", name, p)
		}
		return int64(p["project"].(map[string]any)["id"].(float64)), p
	}
	pid, _ := mkProject("app", storeID, nil)

	// A viewer mints a retrieve-scoped key; /v1 does not read the role.
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
			t.Fatalf("mint key %v: %v", scopes, k)
		}
		return k["api_key"].(string)
	}
	key := mint("retrieve")
	auth := func(k string) map[string]string { return map[string]string{"Authorization": "Bearer " + k} }
	with := func(h map[string]string, kv ...string) map[string]string {
		out := map[string]string{}
		for k, v := range h {
			out[k] = v
		}
		for i := 0; i < len(kv); i += 2 {
			out[kv[i]] = kv[i+1]
		}
		return out
	}
	const tool = "search_fruit_facts"
	callBody := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + tool + `","arguments":{"query":"tell me about banana","top_k":1,"filter":{"lang":"en"}}}}`

	// Legacy: initialize, initialized, tools/list, tools/call -- no session.
	legacy := with(auth(key), "MCP-Protocol-Version", "2025-06-18")
	status, hdr, out := mcpPost(t, srv.URL, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`, auth(key))
	if status != 200 || out["result"] == nil || hdr.Get("Mcp-Session-Id") != "" {
		t.Fatalf("initialize: %d %v session=%q", status, out, hdr.Get("Mcp-Session-Id"))
	}
	if status, _, _ := mcpPost(t, srv.URL, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, legacy); status != 202 {
		t.Errorf("notifications/initialized: %d", status)
	}
	status, _, out = mcpPost(t, srv.URL, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, legacy)
	tools, _ := out["result"].(map[string]any)["tools"].([]any)
	if status != 200 || len(tools) != 1 || tools[0].(map[string]any)["name"] != tool ||
		!strings.Contains(tools[0].(map[string]any)["description"].(string), "Fruit Facts") {
		t.Fatalf("tools/list: %d %v", status, out)
	}
	status, hdr, out = mcpPost(t, srv.URL, callBody, legacy)
	if status != 200 || hdr.Get("Mcp-Session-Id") != "" {
		t.Fatalf("tools/call: %d %v", status, out)
	}
	res := out["result"].(map[string]any)
	text := res["content"].([]any)[0].(map[string]any)["text"].(string)
	if res["isError"] == true || !strings.Contains(text, "Banana") || !strings.Contains(text, `"filename":"fruits.md"`) ||
		!strings.Contains(text, `"lang":"en"`) {
		t.Errorf("call result: %v", res)
	}
	var hits struct {
		Hits []map[string]any `json:"hits"`
	}
	if err := json.Unmarshal([]byte(text), &hits); err != nil || len(hits.Hits) != 1 {
		t.Errorf("hits (top_k 1): %v %s", err, text)
	}

	// The call is accounted like a retrieve; the query is not logged.
	row, found := e.logRow(hdr.Get("X-Request-Id"))
	if !found {
		t.Fatal("no request_logs row for the tools/call")
	}
	if row.endpoint != "retrieve" || row.status != 200 || row.promptTokens <= 0 || row.modelName != "mock-model" {
		t.Errorf("log row: %+v", row)
	}
	var ragUsed bool
	var ragHits int
	if err := e.store.DB().QueryRow(t.Context(), `SELECT rag_used, rag_hits FROM request_logs WHERE request_id = $1`,
		hdr.Get("X-Request-Id")).Scan(&ragUsed, &ragHits); err != nil || !ragUsed || ragHits != 1 {
		t.Errorf("rag columns: used=%v hits=%d err=%v", ragUsed, ragHits, err)
	}
	// The handshake and tools/list open no record.
	var rows int
	if err := e.store.DB().QueryRow(t.Context(), `SELECT count(*) FROM request_logs WHERE project_id = $1`, pid).Scan(&rows); err != nil || rows != 1 {
		t.Errorf("request_logs rows after initialize, list and one call: %d (%v)", rows, err)
	}

	// Modern: server/discover, then tools/call, with the header checks.
	modernMeta := `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}`
	modern := with(auth(key), "MCP-Protocol-Version", "2026-07-28")
	status, hdr, out = mcpPost(t, srv.URL, `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{`+modernMeta+`}}`,
		with(modern, "Mcp-Method", "server/discover"))
	if status != 200 || out["result"] == nil || hdr.Get("Mcp-Session-Id") != "" {
		t.Fatalf("server/discover: %d %v", status, out)
	}
	if out["result"].(map[string]any)["resultType"] != "complete" {
		t.Errorf("server/discover result: %v", out)
	}
	modernCall := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"` + tool + `","arguments":{"query":"banana"},` + modernMeta + `}}`
	status, _, out = mcpPost(t, srv.URL, modernCall, with(modern, "Mcp-Method", "tools/call", "Mcp-Name", tool))
	if status != 200 || out["result"] == nil || out["result"].(map[string]any)["isError"] == true {
		t.Errorf("modern tools/call: %d %v", status, out)
	}
	status, _, out = mcpPost(t, srv.URL, modernCall, with(modern, "Mcp-Method", "tools/call", "Mcp-Name", "search_other"))
	if er, _ := out["error"].(map[string]any); status != 400 || er["code"] != float64(-32020) {
		t.Errorf("Mcp-Name mismatch: %d %v", status, out)
	}
	status, _, out = mcpPost(t, srv.URL, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, with(auth(key), "MCP-Protocol-Version", "2024-11-05"))
	if er, _ := out["error"].(map[string]any); status != 400 || er["code"] != float64(-32022) {
		t.Errorf("unsupported version: %d %v", status, out)
	}
	if resp, err := http.Get(srv.URL + "/mcp"); err != nil || resp.StatusCode != 405 {
		t.Errorf("GET /mcp: %v %v", resp, err)
	}

	// Bad arguments are the tool's answer; a filter outside the grammar too.
	for _, args := range []string{`{"query":""}`, `{"query":"x","top_k":99}`, `{"query":"x","filter":"lang=en"}`} {
		status, _, out = mcpPost(t, srv.URL, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+tool+`","arguments":`+args+`}}`, legacy)
		if res, _ := out["result"].(map[string]any); status != 200 || res["isError"] != true {
			t.Errorf("arguments %s: %d %v", args, status, out)
		}
	}

	// Auth: no key is 401, a key without the retrieve scope is 403, on every
	// method, the handshake included; both bodies are JSON-RPC, id-less.
	for name, c := range map[string]struct {
		hdr  map[string]string
		want int
		code string
	}{
		"no key":      {nil, 401, ""},
		"unknown key": {auth("sk-proj-bogus"), 401, "invalid_api_key"},
		"chat scope":  {auth(mint("chat")), 403, "insufficient_scope"},
		"x-api-key":   {map[string]string{"x-api-key": mint("embeddings")}, 403, "insufficient_scope"},
		"two scopes":  {auth(mint("chat", "embeddings")), 403, "insufficient_scope"},
	} {
		for _, body := range []string{`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`, callBody} {
			status, _, out := mcpPost(t, srv.URL, body, c.hdr)
			er, _ := out["error"].(map[string]any)
			if _, hasID := out["id"]; status != c.want || hasID || out["jsonrpc"] != "2.0" || er["code"] != float64(-32001) {
				t.Errorf("%s: %d %v", name, status, out)
				continue
			}
			if code, _ := er["data"].(map[string]any)["code"].(string); c.code != "" && code != c.code {
				t.Errorf("%s: data.code %q, want %q", name, code, c.code)
			}
		}
	}
	// The x-api-key header carries a usable key too.
	status, _, out = mcpPost(t, srv.URL, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, map[string]string{"x-api-key": key})
	if status != 200 || out["result"] == nil {
		t.Errorf("x-api-key: %d %v", status, out)
	}

	// Origin: a listed one passes, anything else is 403 even with a good key.
	if status, _, _ := mcpPost(t, srv.URL, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, with(legacy, "Origin", "https://app.example")); status != 200 {
		t.Errorf("listed origin: %d", status)
	}
	status, _, out = mcpPost(t, srv.URL, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, with(legacy, "Origin", "https://evil.example"))
	if er, _ := out["error"].(map[string]any); status != 403 || er == nil {
		t.Errorf("unlisted origin: %d %v", status, out)
	}

	// A project without a store lists no tool, and a call to one is an error.
	_, bare := mkProject("bare", 0, nil)
	bareHdr := auth(bare["api_key"].(string))
	status, _, out = mcpPost(t, srv.URL, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, bareHdr)
	if tl, ok := out["result"].(map[string]any)["tools"].([]any); status != 200 || !ok || len(tl) != 0 {
		t.Errorf("tools/list without a store: %d %v", status, out)
	}

	// A store whose name has no ASCII letters is named by its id.
	nonASCII := mkStore("ドキュメント", "# Cherry\n\nCherries are small and sweet.", "")
	_, na := mkProject("jp", nonASCII, nil)
	status, _, out = mcpPost(t, srv.URL, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, auth(na["api_key"].(string)))
	if tl, _ := out["result"].(map[string]any)["tools"].([]any); status != 200 || len(tl) != 1 ||
		tl[0].(map[string]any)["name"] != "search_store_"+strconv.FormatInt(nonASCII, 10) {
		t.Errorf("non-ASCII store name: %d %v", status, out)
	}

	// A user key that grants several projects has to name one, as for chat.
	pid2, _ := mkProject("second", storeID, nil)
	multi := e.call("POST", "/admin/api/keys", map[string]any{"kind": "gateway", "name": "multi", "scopes": []string{"retrieve"},
		"project_ids": []int64{pid, pid2}}, "")
	if multi["_status"] != float64(201) {
		t.Fatalf("mint multi-project key: %v", multi)
	}
	status, hdr, out = mcpPost(t, srv.URL, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, auth(multi["api_key"].(string)))
	if er, _ := out["error"].(map[string]any); status != 400 || er["code"] != float64(-32001) || hdr.Get("X-Ragmux-Projects") == "" {
		t.Errorf("several projects, none named: %d %v %v", status, out, hdr)
	}
	status, _, out = mcpPost(t, srv.URL, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		with(auth(multi["api_key"].(string)), "X-Ragmux-Project", "second"))
	if status != 200 || out["result"] == nil {
		t.Errorf("several projects, one named: %d %v", status, out)
	}

	// The project's rate limit applies to tools/call: 429, Retry-After and a
	// JSON-RPC body.
	_, limited := mkProject("limited", storeID, map[string]any{"rate_limit_rpm": 1})
	limHdr := auth(limited["api_key"].(string))
	if status, _, out := mcpPost(t, srv.URL, callBody, limHdr); status != 200 || out["result"] == nil {
		t.Fatalf("first call under the limit: %d %v", status, out)
	}
	status, hdr, out = mcpPost(t, srv.URL, callBody, limHdr)
	if er, _ := out["error"].(map[string]any); status != 429 || hdr.Get("Retry-After") == "" || er == nil || er["code"] != float64(-32001) {
		t.Errorf("second call over the limit: %d retry=%q %v", status, hdr.Get("Retry-After"), out)
	}
}
