package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ragmux/ragmux/internal/gateway"
)

// fakeBackend stands in for the gateway: it has no database, so the protocol
// layer is tested on its own. The refusals the real gateway writes are
// covered by TestGatewayRefusalsAreJSONRPC and the e2e tests.
type fakeBackend struct {
	store  *gateway.MCPStore
	result *gateway.MCPSearchResult
	fail   *gateway.MCPFailure
	// header is set on the response by MCPSearch, as the limit step does.
	header [2]string
	args   []gateway.MCPSearchArgs
	authed int
}

func (f *fakeBackend) MCPAuth(gateway.MCPRefuseFunc) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			f.authed++
			next.ServeHTTP(w, r)
		})
	}
}

func (f *fakeBackend) MCPStoreFor(context.Context) (*gateway.MCPStore, error) { return f.store, nil }

func (f *fakeBackend) MCPSearch(w http.ResponseWriter, _ *http.Request, a gateway.MCPSearchArgs) (*gateway.MCPSearchResult, *gateway.MCPFailure) {
	if a.Query == "" { // the gateway's own validation answers this one
		return nil, &gateway.MCPFailure{Status: 400, Code: "invalid_request_error", Message: "query is required", Tool: true}
	}
	f.args = append(f.args, a)
	if f.header[0] != "" {
		w.Header().Set(f.header[0], f.header[1])
	}
	return f.result, f.fail
}

func newFake() *fakeBackend {
	return &fakeBackend{
		store: &gateway.MCPStore{ID: 7, Name: "Product Docs", DocumentCount: 3},
		result: &gateway.MCPSearchResult{Hits: []gateway.MCPHit{{Content: "Bananas are yellow.", Score: 0.9,
			Filename: "fruit.md", Section: "Banana", DocumentID: 1, ChunkID: 2, DocumentMetadata: json.RawMessage(`{"lang":"en"}`)}}},
	}
}

const toolSearch = "search_product_docs"

func serve(t *testing.T, f *fakeBackend, origins ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(New(Config{Backend: f, Version: "test", AllowedOrigins: origins}))
	t.Cleanup(srv.Close)
	return srv
}

// post sends one JSON-RPC body with the given headers and decodes the answer.
func post(t *testing.T, url, body string, hdr map[string]string) (int, http.Header, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
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
			t.Fatalf("response is not JSON (%d): %q", resp.StatusCode, raw)
		}
	}
	return resp.StatusCode, resp.Header, out
}

func rpcErr(t *testing.T, out map[string]any) (int, map[string]any) {
	t.Helper()
	e, ok := out["error"].(map[string]any)
	if !ok {
		t.Fatalf("not a JSON-RPC error: %v", out)
	}
	return int(e["code"].(float64)), e
}

// modernBody is a modern request: the version rides in params._meta.
func modernBody(method string, params map[string]any) string {
	if params == nil {
		params = map[string]any{}
	}
	params["_meta"] = map[string]any{metaVersionKey: versionModern}
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	return string(b)
}

func modernHeaders(method, name string) map[string]string {
	h := map[string]string{"MCP-Protocol-Version": versionModern, "Mcp-Method": method}
	if name != "" {
		h["Mcp-Name"] = name
	}
	return h
}

func resultText(t *testing.T, out map[string]any) (string, bool) {
	t.Helper()
	res, ok := out["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result: %v", out)
	}
	content := res["content"].([]any)
	isErr, _ := res["isError"].(bool)
	return content[0].(map[string]any)["text"].(string), isErr
}

func TestToolName(t *testing.T) {
	long := strings.Repeat("a", 60)
	for _, tc := range []struct {
		name string
		id   int64
		want string
	}{
		{"Product Docs", 1, "search_product_docs"},
		{"  --Docs & FAQ!! ", 1, "search_docs_faq"},
		{"a__b", 1, "search_a_b"},
		{"v2.1-notes", 1, "search_v2_1_notes"},
		{"Müşteri", 1, "search_m_teri"},
		{"ドキュメント", 12, "search_store_12"},
		{"", 5, "search_store_5"},
		{"___", 5, "search_store_5"},
		{long, 1, "search_" + strings.Repeat("a", 41)},
		// Cut at 48 characters, and the underscore the cut leaves is trimmed.
		{strings.Repeat("a", 40) + " b", 1, "search_" + strings.Repeat("a", 40)},
	} {
		got := ToolName(tc.name, tc.id)
		if got != tc.want {
			t.Errorf("ToolName(%q) = %q, want %q", tc.name, got, tc.want)
		}
		if len(got) > maxToolNameLen {
			t.Errorf("ToolName(%q) is %d characters", tc.name, len(got))
		}
	}
}

func TestLegacyFlow(t *testing.T) {
	f := newFake()
	srv := serve(t, f)

	status, hdr, out := post(t, srv.URL, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"c","version":"1"}}}`, nil)
	if status != 200 {
		t.Fatalf("initialize: %d %v", status, out)
	}
	if hdr.Get("Mcp-Session-Id") != "" {
		t.Errorf("initialize minted a session: %q", hdr.Get("Mcp-Session-Id"))
	}
	res := out["result"].(map[string]any)
	if res["protocolVersion"] != "2025-06-18" || res["serverInfo"].(map[string]any)["name"] != "ragmux" {
		t.Errorf("initialize result: %v", res)
	}
	if _, ok := res["capabilities"].(map[string]any)["tools"]; !ok {
		t.Errorf("capabilities: %v", res["capabilities"])
	}
	// A version the server does not know is answered with its newest.
	_, _, out = post(t, srv.URL, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}`, nil)
	if out["result"].(map[string]any)["protocolVersion"] != "2025-11-25" {
		t.Errorf("unknown requested version: %v", out)
	}

	status, hdr, _ = post(t, srv.URL, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, map[string]string{"MCP-Protocol-Version": "2025-06-18", "Mcp-Session-Id": "ignored"})
	if status != 202 || hdr.Get("Mcp-Session-Id") != "" {
		t.Errorf("notifications/initialized: %d session=%q", status, hdr.Get("Mcp-Session-Id"))
	}
	status, _, out = post(t, srv.URL, `{"jsonrpc":"2.0","id":9,"method":"ping"}`, map[string]string{"MCP-Protocol-Version": "2025-06-18"})
	if status != 200 || out["result"] == nil || out["id"] != float64(9) {
		t.Errorf("ping: %d %v", status, out)
	}

	h := map[string]string{"MCP-Protocol-Version": "2025-06-18", "Last-Event-ID": "5"}
	status, hdr, out = post(t, srv.URL, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, h)
	if status != 200 || hdr.Get("Mcp-Session-Id") != "" {
		t.Fatalf("tools/list: %d %v", status, out)
	}
	tools := out["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools: %v", tools)
	}
	tl := tools[0].(map[string]any)
	if tl["name"] != toolSearch || !strings.Contains(tl["description"].(string), "Product Docs") ||
		!strings.Contains(tl["description"].(string), "3 documents") {
		t.Errorf("tool: %v", tl)
	}
	schema := tl["inputSchema"].(map[string]any)
	if schema["required"].([]any)[0] != "query" || schema["properties"].(map[string]any)["filter"] == nil {
		t.Errorf("schema: %v", schema)
	}

	status, hdr, out = post(t, srv.URL, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"`+toolSearch+`","arguments":{"query":"banana","top_k":2,"filter":{"lang":"en"}}}}`, h)
	if status != 200 || hdr.Get("Mcp-Session-Id") != "" {
		t.Fatalf("tools/call: %d %v", status, out)
	}
	text, isErr := resultText(t, out)
	if isErr || !strings.Contains(text, "Bananas are yellow.") || !strings.Contains(text, `"filename":"fruit.md"`) {
		t.Errorf("call result: %q err=%v", text, isErr)
	}
	if len(f.args) != 1 || f.args[0].Query != "banana" || f.args[0].TopK != 2 || string(f.args[0].Filter) != `{"lang":"en"}` {
		t.Errorf("args reached the gateway as %+v", f.args)
	}

	// A legacy client that sends no version header at all is a legacy client.
	status, _, out = post(t, srv.URL, `{"jsonrpc":"2.0","id":4,"method":"tools/list"}`, nil)
	if status != 200 || out["result"] == nil {
		t.Errorf("tools/list without a version header: %d %v", status, out)
	}
	// The modern header checks do not apply to it: no Mcp-Method, no Mcp-Name.
	status, _, out = post(t, srv.URL, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"`+toolSearch+`","arguments":{"query":"x"}}}`, map[string]string{"MCP-Protocol-Version": "2025-03-26"})
	if status != 200 || out["result"] == nil {
		t.Errorf("legacy tools/call without Mcp-Name: %d %v", status, out)
	}
	if f.authed == 0 {
		t.Error("requests did not pass the gateway's authentication")
	}
}

func TestModernFlow(t *testing.T) {
	f := newFake()
	srv := serve(t, f)

	status, hdr, out := post(t, srv.URL, modernBody("server/discover", nil), modernHeaders("server/discover", ""))
	if status != 200 || hdr.Get("Mcp-Session-Id") != "" {
		t.Fatalf("server/discover: %d %v", status, out)
	}
	res := out["result"].(map[string]any)
	versions := res["supportedVersions"].([]any)
	info, _ := res["_meta"].(map[string]any)[metaServerInfoKey].(map[string]any)
	if versions[0] != versionModern || len(versions) != 4 || info["name"] != "ragmux" || info["version"] != "test" {
		t.Errorf("discover result: %v", res)
	}
	if _, top := res["serverInfo"]; top || res["resultType"] != "complete" || res["capabilities"] == nil || res["instructions"] == "" {
		t.Errorf("discover result shape: %v", res)
	}

	status, _, out = post(t, srv.URL, modernBody("tools/list", nil), modernHeaders("tools/list", ""))
	if status != 200 || len(out["result"].(map[string]any)["tools"].([]any)) != 1 ||
		out["result"].(map[string]any)["resultType"] != "complete" {
		t.Fatalf("tools/list: %d %v", status, out)
	}

	body := modernBody("tools/call", map[string]any{"name": toolSearch, "arguments": map[string]any{"query": "banana"}})
	status, _, out = post(t, srv.URL, body, modernHeaders("tools/call", toolSearch))
	if status != 200 {
		t.Fatalf("tools/call: %d %v", status, out)
	}
	if text, isErr := resultText(t, out); isErr || !strings.Contains(text, "Bananas") {
		t.Errorf("call result: %q", text)
	}
	if out["result"].(map[string]any)["resultType"] != "complete" {
		t.Errorf("tools/call result has no resultType: %v", out)
	}
	// A tool error is a result too.
	body = modernBody("tools/call", map[string]any{"name": toolSearch, "arguments": map[string]any{}})
	_, _, out = post(t, srv.URL, body, modernHeaders("tools/call", toolSearch))
	if res := out["result"].(map[string]any); res["isError"] != true || res["resultType"] != "complete" {
		t.Errorf("tool error result: %v", out)
	}
}

// resultType belongs to the modern era; a legacy result is left as it was.
func TestLegacyResultsHaveNoResultType(t *testing.T) {
	srv := serve(t, newFake())
	for _, body := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","id":1,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + toolSearch + `","arguments":{"query":"q"}}}`,
	} {
		_, _, out := post(t, srv.URL, body, nil)
		if _, has := out["result"].(map[string]any)["resultType"]; has {
			t.Errorf("legacy result carries resultType: %v", out)
		}
	}
}

func b64Header(v string) string {
	return "=?base64?" + base64.StdEncoding.EncodeToString([]byte(v)) + "?="
}

// A header value that is not plain ASCII arrives as =?base64?...?= and is
// decoded before it is compared with the body.
func TestMcpNameBase64(t *testing.T) {
	srv := serve(t, newFake())
	body := modernBody("tools/call", map[string]any{"name": toolSearch, "arguments": map[string]any{"query": "x"}})
	for _, tc := range []struct {
		name, header string
		status       int
	}{
		{"encoded and equal", b64Header(toolSearch), 200},
		{"encoded and different", b64Header("search_other"), 400},
		{"not base64", "=?base64?!!!?=", 400},
		{"encoded, not UTF-8", "=?base64?/w==?=", 400},
	} {
		h := modernHeaders("tools/call", tc.header)
		status, _, out := post(t, srv.URL, body, h)
		if status != tc.status {
			t.Errorf("%s: %d %v", tc.name, status, out)
		}
		if tc.status == 400 {
			if code, _ := rpcErr(t, out); code != codeHeaderMismatch {
				t.Errorf("%s: code %d", tc.name, code)
			}
		}
	}
	// Only Mcp-Name takes the encoded form; the method is compared as sent.
	h := modernHeaders("tools/call", toolSearch)
	h["Mcp-Method"] = b64Header("tools/call")
	if status, _, out := post(t, srv.URL, body, h); status != 400 {
		t.Errorf("an encoded Mcp-Method was accepted: %d %v", status, out)
	}
}

func TestModernHeaderErrors(t *testing.T) {
	srv := serve(t, newFake())
	call := modernBody("tools/call", map[string]any{"name": toolSearch, "arguments": map[string]any{"query": "x"}})
	for _, tc := range []struct {
		name   string
		body   string
		hdr    map[string]string
		status int
		code   int
	}{
		{"Mcp-Name differs from the tool", call, modernHeaders("tools/call", "search_other"), 400, codeHeaderMismatch},
		{"Mcp-Name missing", call, modernHeaders("tools/call", ""), 400, codeHeaderMismatch},
		{"Mcp-Method differs", modernBody("tools/list", nil), modernHeaders("tools/call", ""), 400, codeHeaderMismatch},
		{"Mcp-Method missing", modernBody("tools/list", nil), map[string]string{"MCP-Protocol-Version": versionModern}, 400, codeHeaderMismatch},
		{"version header missing", modernBody("tools/list", nil), map[string]string{"Mcp-Method": "tools/list"}, 400, codeHeaderMismatch},
		{"version header differs from _meta", modernBody("tools/list", nil),
			map[string]string{"MCP-Protocol-Version": "2025-06-18", "Mcp-Method": "tools/list"}, 400, codeHeaderMismatch},
		{"modern header without _meta", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, modernHeaders("tools/list", ""), 400, codeHeaderMismatch},
		{"unsupported header version", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
			map[string]string{"MCP-Protocol-Version": "2024-11-05"}, 400, codeUnsupportedVers},
		{"unsupported version on initialize", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}`,
			map[string]string{"MCP-Protocol-Version": "2024-11-05"}, 400, codeUnsupportedVers},
		{"unsupported _meta version", strings.Replace(modernBody("tools/list", nil), versionModern, "2099-01-01", 1),
			map[string]string{"MCP-Protocol-Version": "2099-01-01", "Mcp-Method": "tools/list"}, 400, codeUnsupportedVers},
	} {
		status, _, out := post(t, srv.URL, tc.body, tc.hdr)
		if status != tc.status {
			t.Errorf("%s: status %d, want %d (%v)", tc.name, status, tc.status, out)
			continue
		}
		code, e := rpcErr(t, out)
		if code != tc.code {
			t.Errorf("%s: code %d, want %d", tc.name, code, tc.code)
		}
		if tc.code == codeUnsupportedVers {
			sup := e["data"].(map[string]any)["supported"].([]any)
			if len(sup) != 4 || sup[0] != versionModern {
				t.Errorf("%s: supported = %v", tc.name, sup)
			}
		}
	}
}

func TestMethodsAndEnvelope(t *testing.T) {
	srv := serve(t, newFake())

	for _, m := range []string{http.MethodGet, http.MethodDelete, http.MethodPut} {
		req, _ := http.NewRequest(m, srv.URL, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != 405 || resp.Header.Get("Allow") != "POST" {
			t.Errorf("%s: %d allow=%q", m, resp.StatusCode, resp.Header.Get("Allow"))
		}
	}

	// An unknown method: 404 for a modern request, a JSON-RPC error in a 200
	// for a legacy one.
	status, _, out := post(t, srv.URL, modernBody("resources/list", nil), modernHeaders("resources/list", ""))
	if code, _ := rpcErr(t, out); status != 404 || code != codeMethodNotFound {
		t.Errorf("modern unknown method: %d %v", status, out)
	}
	status, _, out = post(t, srv.URL, `{"jsonrpc":"2.0","id":1,"method":"resources/list"}`, nil)
	if code, _ := rpcErr(t, out); status != 200 || code != codeMethodNotFound {
		t.Errorf("legacy unknown method: %d %v", status, out)
	}
	// initialize and ping are not modern methods.
	status, _, out = post(t, srv.URL, modernBody("ping", nil), modernHeaders("ping", ""))
	if code, _ := rpcErr(t, out); status != 404 || code != codeMethodNotFound {
		t.Errorf("modern ping: %d %v", status, out)
	}

	for name, body := range map[string]string{
		"batch":     `[{"jsonrpc":"2.0","id":1,"method":"ping"}]`,
		"garbage":   `{not json`,
		"no rpc":    `{"id":1,"method":"ping"}`,
		"null id":   `{"jsonrpc":"2.0","id":null,"method":"ping"}`,
		"no method": `{"jsonrpc":"2.0","id":1}`,
	} {
		status, _, out := post(t, srv.URL, body, nil)
		if status != 400 {
			t.Errorf("%s: %d %v", name, status, out)
		}
		rpcErr(t, out)
	}
	// A client's answer to a request is accepted and ignored.
	if status, _, _ := post(t, srv.URL, `{"jsonrpc":"2.0","id":1,"result":{}}`, nil); status != 202 {
		t.Errorf("client response: %d", status)
	}
}

func TestOrigin(t *testing.T) {
	ping := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	for _, tc := range []struct {
		name    string
		origins []string
		origin  string
		want    int
	}{
		{"no origin, empty list", nil, "", 200},
		{"origin, empty list", nil, "https://evil.example", 403},
		{"listed origin", []string{"https://app.example"}, "https://app.example", 200},
		{"unlisted origin", []string{"https://app.example"}, "https://evil.example", 403},
		{"wildcard", []string{"*"}, "https://anything.example", 200},
		{"no origin, list", []string{"https://app.example"}, "", 200},
	} {
		srv := serve(t, newFake(), tc.origins...)
		h := map[string]string{}
		if tc.origin != "" {
			h["Origin"] = tc.origin
		}
		status, _, out := post(t, srv.URL, ping, h)
		if status != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, status, tc.want)
		}
		if tc.want == 403 {
			rpcErr(t, out)
		}
	}
	// The origin check comes before the method check.
	srv := serve(t, newFake())
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req.Header.Set("Origin", "https://evil.example")
	resp, _ := http.DefaultClient.Do(req)
	_ = resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Errorf("GET with a bad origin: %d", resp.StatusCode)
	}
}

func TestToolsWithoutStore(t *testing.T) {
	f := newFake()
	f.store = nil
	srv := serve(t, f)
	_, _, out := post(t, srv.URL, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, nil)
	tools, ok := out["result"].(map[string]any)["tools"].([]any)
	if !ok || len(tools) != 0 {
		t.Errorf("a project without a store lists %v", out)
	}
	status, _, out := post(t, srv.URL, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_x","arguments":{"query":"q"}}}`, nil)
	if code, _ := rpcErr(t, out); status != 200 || code != codeInvalidParams {
		t.Errorf("call without a store: %d %v", status, out)
	}
}

func TestToolArguments(t *testing.T) {
	f := newFake()
	srv := serve(t, f)
	call := func(args string) (int, map[string]any) {
		status, _, out := post(t, srv.URL, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+toolSearch+`","arguments":`+args+`}}`, nil)
		return status, out
	}

	// Wrong arguments are the tool's answer, so the model can correct them.
	for _, args := range []string{`{}`, `{"top_k":3}`, `{"query":"q","top_k":0}`, `{"query":"q","top_k":51}`,
		`{"query":5}`, `{"query":"q","top_k":"3"}`, `[]`, `{"query":"q","limit":3}`} {
		status, out := call(args)
		if text, isErr := resultText(t, out); status != 200 || !isErr {
			t.Errorf("%s: %d %q err=%v", args, status, text, isErr)
		}
	}
	if len(f.args) != 0 {
		t.Errorf("invalid arguments reached the gateway: %+v", f.args)
	}

	// null for an optional argument is no argument.
	if _, out := call(`{"query":"q","top_k":null,"filter":null}`); out["result"] == nil {
		t.Fatalf("null optionals: %v", out)
	}
	if a := f.args[0]; a.TopK != 0 || a.Filter != nil {
		t.Errorf("null optionals reached the gateway as %+v", a)
	}

	// A missing arguments member is an empty argument set.
	status, _, out := post(t, srv.URL, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+toolSearch+`"}}`, nil)
	if _, isErr := resultText(t, out); status != 200 || !isErr {
		t.Errorf("no arguments: %d %v", status, out)
	}

	// An unknown tool is a protocol error, and its name is clipped in the echo.
	_, _, out = post(t, srv.URL, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+strings.Repeat("z", 500)+`"}}`, nil)
	code, e := rpcErr(t, out)
	if code != codeInvalidParams || len(e["message"].(string)) > 100 {
		t.Errorf("unknown tool: %v", out)
	}
	_, _, out = post(t, srv.URL, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{}}`, nil)
	if code, _ := rpcErr(t, out); code != codeInvalidParams {
		t.Errorf("no tool name: %v", out)
	}
}

func TestSearchFailures(t *testing.T) {
	f := newFake()
	srv := serve(t, f)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + toolSearch + `","arguments":{"query":"q"}}}`

	// A search that failed is a tool result with isError, a 200.
	f.result, f.fail = nil, &gateway.MCPFailure{Status: 502, Code: "upstream_error", Message: "embedding failed", Tool: true}
	f.header = [2]string{"Retry-After", "3"}
	status, hdr, out := post(t, srv.URL, body, nil)
	if text, isErr := resultText(t, out); status != 200 || !isErr || text != "embedding failed" || hdr.Get("Retry-After") != "" {
		t.Errorf("tool failure: %d %q err=%v retry=%q", status, text, isErr, hdr.Get("Retry-After"))
	}

	// A refused call keeps its status and the headers the gateway set.
	f.fail = &gateway.MCPFailure{Status: 429, Code: "rate_limit_exceeded", Message: "rate limit exceeded"}
	f.header = [2]string{"Retry-After", "7"}
	status, hdr, out = post(t, srv.URL, body, nil)
	code, e := rpcErr(t, out)
	if status != 429 || hdr.Get("Retry-After") != "7" || code != codeRefused ||
		e["data"].(map[string]any)["code"] != "rate_limit_exceeded" || out["id"] != float64(1) {
		t.Errorf("limit refusal: %d %v retry=%q", status, out, hdr.Get("Retry-After"))
	}

	f.fail = &gateway.MCPFailure{Status: 500, Code: "server_error", Message: "model connection unavailable"}
	status, _, out = post(t, srv.URL, body, nil)
	if code, _ := rpcErr(t, out); status != 500 || code != codeInternalError {
		t.Errorf("server refusal: %d %v", status, out)
	}
}

// TestGatewayRefusalsAreJSONRPC runs the real gateway's authentication in
// front of the server. A request without a usable credential never reaches
// the store, so no database is needed; the scope and project refusals, which
// need one, are in internal/e2e.
func TestGatewayRefusalsAreJSONRPC(t *testing.T) {
	gw := &gateway.Gateway{}
	srv := httptest.NewServer(New(Config{Backend: gw}))
	t.Cleanup(srv.Close)
	ping := `{"jsonrpc":"2.0","id":1,"method":"ping"}`

	for name, hdr := range map[string]map[string]string{
		"no credential":       nil,
		"not a ragmux key":    {"Authorization": "Bearer nope"},
		"x-api-key as bearer": {"x-api-key": "nope"},
	} {
		status, h, out := post(t, srv.URL, ping, hdr)
		if status != 401 {
			t.Errorf("%s: %d %v", name, status, out)
			continue
		}
		code, e := rpcErr(t, out)
		if _, hasID := out["id"]; hasID || code != codeRefused || out["jsonrpc"] != "2.0" ||
			e["message"] == "" || e["data"].(map[string]any)["code"] == "" {
			t.Errorf("%s: body %v", name, out)
		}
		if h.Get("Content-Type") != "application/json" {
			t.Errorf("%s: content type %q", name, h.Get("Content-Type"))
		}
	}

	// The key is unknown in the same words whichever header carried it.
	_, _, a := post(t, srv.URL, ping, map[string]string{"Authorization": "Bearer nope"})
	_, _, b := post(t, srv.URL, ping, map[string]string{"x-api-key": "other"})
	if a["error"].(map[string]any)["message"] != b["error"].(map[string]any)["message"] {
		t.Errorf("unknown keys are told apart: %v / %v", a, b)
	}
	if a["error"].(map[string]any)["data"].(map[string]any)["code"] != "invalid_api_key" {
		t.Errorf("unknown key code: %v", a)
	}
}
