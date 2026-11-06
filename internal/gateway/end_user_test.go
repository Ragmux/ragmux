package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ragmux/ragmux/internal/limits"
	"github.com/ragmux/ragmux/internal/provider"
	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/internal/tracing"
	"github.com/ragmux/ragmux/internal/tracing/tracingtest"
)

// End users, tags and the per-end-user limit tier (ADR-035).

// hashedEndUser is what the Anthropic adapter is expected to send for v.
func hashedEndUser(v string) string {
	sum := sha256.Sum256([]byte(v))
	return "h-" + hex.EncodeToString(sum[:])[:61]
}

// chatH posts a chat request with the headers exactly as given, so a header can
// be sent twice.
func (e *env) chatH(h http.Header, body any) (*http.Response, map[string]any) {
	e.t.Helper()
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, e.srv.URL+"/v1/chat/completions", bytes.NewReader(raw))
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.key)
	for k, vs := range h {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	return resp, m
}

// waitEndUser waits for the deferred end-user counter write to reach requests.
func (e *env) waitEndUser(user string, requests int) store.UsageRow {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, day, _, err := e.st.GetEndUserUsage(context.Background(), e.proj.ID, user, limits.Windows(time.Now()))
		if err != nil {
			e.t.Fatal(err)
		}
		if day.Requests >= requests || time.Now().After(deadline) {
			return day
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// endUserRows counts the end_user_usage rows of the project.
func (e *env) endUserRows() int {
	e.t.Helper()
	var n int
	if err := e.st.DB().QueryRow(context.Background(), `SELECT count(*) FROM end_user_usage WHERE project_id = $1`, e.proj.ID).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func warningsOf(resp *http.Response) string { return strings.Join(resp.Header.Values("Warning"), "\n") }

func TestEndUserSourcePriorityOnChat(t *testing.T) {
	const hdr = "Ragmux-Header-User"
	cases := []struct {
		name    string
		headers map[string]string
		extra   map[string]any
		want    string // the end user of record
		user    any    // what the OpenAI-compatible upstream saw as "user"
		safety  any    // ... as "safety_identifier"
	}{
		{"none", nil, nil, "", nil, nil},
		{"header only", map[string]string{"X-Ragmux-End-User": hdr}, nil, hdr, nil, nil},
		{"user only", nil, map[string]any{"user": "u-1"}, "u-1", "u-1", nil},
		{"safety_identifier only", nil, map[string]any{"safety_identifier": "s-1"}, "s-1", nil, "s-1"},
		{"safety_identifier before user", nil, map[string]any{"safety_identifier": "s-1", "user": "u-1"}, "s-1", "u-1", "s-1"},
		{"header before the body", map[string]string{"X-Ragmux-End-User": hdr}, map[string]any{"safety_identifier": "s-1", "user": "u-1"}, hdr, "u-1", "s-1"},
		{"empty safety_identifier falls through to user", nil, map[string]any{"safety_identifier": "", "user": "u-1"}, "u-1", "u-1", ""},
		{"email address", nil, map[string]any{"user": "ali@firma.com"}, "ali@firma.com", "ali@firma.com", nil},
	}
	for _, tc := range cases {
		e := newEnv(t, nil)
		seen := e.recordUpstream(nil)
		body := map[string]any{"messages": userMsg}
		for k, v := range tc.extra {
			body[k] = v
		}
		resp, out := e.chatWith(tc.headers, body)
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d %v", tc.name, resp.StatusCode, out)
		}
		if rec := e.lastLog(); rec.EndUser != tc.want {
			t.Errorf("%s: end_user = %q, want %q", tc.name, rec.EndUser, tc.want)
		}
		up := seen.last()
		if up["user"] != tc.user || up["safety_identifier"] != tc.safety {
			t.Errorf("%s: the upstream saw user %v safety_identifier %v, want %v and %v (the body goes through as it is)",
				tc.name, up["user"], up["safety_identifier"], tc.user, tc.safety)
		}
		// The header is Ragmux's own and never leaves, as a header or in the body.
		if raw, _ := json.Marshal(up); strings.Contains(string(raw), hdr) {
			t.Errorf("%s: the header's end user reached the upstream: %s", tc.name, raw)
		}
		if len(resp.Header.Values("Warning")) != 0 {
			t.Errorf("%s: unexpected warnings %v", tc.name, resp.Header.Values("Warning"))
		}
	}
}

func TestEndUserFromMessagesMetadata(t *testing.T) {
	// A Messages request served by an OpenAI-compatible upstream is translated:
	// metadata.user_id becomes the upstream's "user", as it always has.
	e := newEnv(t, nil)
	seen := e.recordUpstream(nil)
	body := map[string]any{"model": "claude-x", "max_tokens": 32, "messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"metadata": map[string]any{"user_id": "meta-1"}}
	resp, raw := e.messages(body, map[string]string{"x-api-key": e.key})
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	if rec := e.lastLog(); rec.EndUser != "meta-1" {
		t.Errorf("end_user = %q, want meta-1", rec.EndUser)
	}
	if got := seen.last()["user"]; got != "meta-1" {
		t.Errorf("the translated upstream saw user %v, want meta-1", got)
	}

	// The header wins and does not go upstream.
	resp, raw = e.messages(body, map[string]string{"x-api-key": e.key, "X-Ragmux-End-User": "hdr-2"})
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	if rec := e.lastLog(); rec.EndUser != "hdr-2" {
		t.Errorf("end_user = %q, want hdr-2", rec.EndUser)
	}
	if b, _ := json.Marshal(seen.last()); strings.Contains(string(b), "hdr-2") || seen.last()["user"] != "meta-1" {
		t.Errorf("upstream body = %s", b)
	}

	// An unusable user_id is not the end user, earns a Warning and the request
	// is served; the translated upstream still gets it as it was sent.
	body["metadata"] = map[string]any{"user_id": "has space"}
	resp, raw = e.messages(body, map[string]string{"x-api-key": e.key})
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	if !strings.Contains(warningsOf(resp), "metadata.user_id is not a usable end user id") {
		t.Errorf("Warning = %v", resp.Header.Values("Warning"))
	}
	if rec := e.lastLog(); rec.EndUser != "" {
		t.Errorf("end_user = %q, want none", rec.EndUser)
	}
	if got := seen.last()["user"]; got != "has space" {
		t.Errorf("the translated upstream saw user %v, want the client's value as it was", got)
	}
}

func TestEndUserOnTheNativeMessagesPathGoesThroughAsItIs(t *testing.T) {
	e, up := anthropicEnv(t, "claude-opus-5-5", jsonAnswer(anthropicAnswer))
	body := map[string]any{"model": "claude-opus-5-5", "max_tokens": 32, "messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"metadata": map[string]any{"user_id": "ali@firma.com"}}
	resp, raw := e.messages(body, map[string]string{"x-api-key": e.key, "X-Ragmux-End-User": "hdr-3"})
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	b := up.lastBody(t)
	md, _ := b["metadata"].(map[string]any)
	if md["user_id"] != "ali@firma.com" {
		t.Errorf("native upstream metadata = %v, want the client's own user_id untouched", b["metadata"])
	}
	if enc, _ := json.Marshal(b); strings.Contains(string(enc), "hdr-3") {
		t.Errorf("the header's end user reached the native upstream: %s", enc)
	}
	if rec := e.lastLog(); rec.EndUser != "hdr-3" {
		t.Errorf("end_user = %q, want hdr-3", rec.EndUser)
	}

	// A user_id the end-user grammar refuses is still the client's own field.
	body["metadata"] = map[string]any{"user_id": strings.Repeat("x", 600)}
	if resp, raw := e.messages(body, map[string]string{"x-api-key": e.key}); resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	md, _ = up.lastBody(t)["metadata"].(map[string]any)
	if s, _ := md["user_id"].(string); len(s) != 600 {
		t.Errorf("native upstream user_id has %d characters, want the 600 the client sent", len(s))
	}
	if rec := e.lastLog(); rec.EndUser != "" {
		t.Errorf("end_user = %q, want none", rec.EndUser)
	}
}

func TestInvalidEndUserHeaderIsRefused(t *testing.T) {
	e := newEnv(t, nil)
	seen := e.recordUpstream(nil)
	cases := map[string]http.Header{
		"space":     {"X-Ragmux-End-User": {"a b"}},
		"quote":     {"X-Ragmux-End-User": {`a"b`}},
		"too long":  {"X-Ragmux-End-User": {strings.Repeat("a", 129)}},
		"empty":     {"X-Ragmux-End-User": {""}},
		"non ascii": {"X-Ragmux-End-User": {"ş"}},
		"twice":     {"X-Ragmux-End-User": {"a", "b"}},
	}
	for name, h := range cases {
		resp, out := e.chatH(h, map[string]any{"messages": userMsg})
		if resp.StatusCode != http.StatusBadRequest || errorField(t, out, "code") != "invalid_end_user" ||
			errorField(t, out, "type") != "invalid_request_error" {
			t.Errorf("%s: %d %v, want 400 invalid_end_user", name, resp.StatusCode, out)
		}
	}
	if seen.count() != 0 {
		t.Errorf("a refused request reached the upstream %d times", seen.count())
	}
	rows, err := e.st.RecentRequests(context.Background(), store.MetricsFilter{ProjectID: &e.proj.ID}, 10)
	if err != nil || len(rows) != 0 {
		t.Errorf("refused requests wrote %d rows (%v)", len(rows), err)
	}
	// 128 bytes of the whole alphabet is the longest and widest value there is.
	widest := strings.Repeat("aZ09._:@+=/-", 11)[:128]
	if resp, out := e.chatH(http.Header{"X-Ragmux-End-User": {widest}}, map[string]any{"messages": userMsg}); resp.StatusCode != 200 {
		t.Errorf("a 128-byte value: %d %v", resp.StatusCode, out)
	}
	if rec := e.lastLog(); rec.EndUser != widest {
		t.Errorf("end_user = %q", rec.EndUser)
	}
}

func TestInvalidBodyEndUserWarnsAndIsServed(t *testing.T) {
	cases := []struct {
		name  string
		extra map[string]any
		warn  string
		user  any // what the OpenAI-compatible upstream saw
	}{
		{"user with a space", map[string]any{"user": "a b"}, "user is not a usable end user id", "a b"},
		{"user too long", map[string]any{"user": strings.Repeat("u", 129)}, "user is not a usable end user id", strings.Repeat("u", 129)},
		{"invalid safety_identifier does not fall through to user", map[string]any{"safety_identifier": "a b", "user": "ok-1"},
			"safety_identifier is not a usable end user id", "ok-1"},
		{"safety_identifier of the wrong type", map[string]any{"safety_identifier": 123, "user": "ok-1"},
			"safety_identifier is not a usable end user id", "ok-1"},
	}
	for _, tc := range cases {
		e := newEnv(t, nil)
		seen := e.recordUpstream(nil)
		body := map[string]any{"messages": userMsg}
		for k, v := range tc.extra {
			body[k] = v
		}
		resp, out := e.chat(body)
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d %v", tc.name, resp.StatusCode, out)
		}
		if !strings.Contains(warningsOf(resp), tc.warn) {
			t.Errorf("%s: Warning = %v, want %q", tc.name, resp.Header.Values("Warning"), tc.warn)
		}
		if rec := e.lastLog(); rec.EndUser != "" {
			t.Errorf("%s: end_user = %q, want none", tc.name, rec.EndUser)
		}
		if got := seen.last()["user"]; got != tc.user {
			t.Errorf("%s: the upstream saw user %v, want it as it was sent", tc.name, got)
		}
	}
}

// anthropicChatEnv is an env whose project is served by an Anthropic connection
// on the chat endpoint, which translates the request.
func anthropicChatEnv(t *testing.T, mutate func(*store.Project)) (*env, *scripted) {
	t.Helper()
	e, up := anthropicEnv(t, "claude-sonnet-4-5", jsonAnswer(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],`+
		`"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":1}}`))
	if mutate != nil {
		mutate(e.proj)
		if _, err := e.st.UpdateProject(context.Background(), e.proj); err != nil {
			t.Fatal(err)
		}
	}
	return e, up
}

func TestAnthropicTranslationSendsTheHashedBodyEndUser(t *testing.T) {
	long := strings.Repeat("u", 600)
	cases := []struct {
		name    string
		headers map[string]string
		extra   map[string]any
		want    string // the value the provider hashes; "" means no metadata at all
	}{
		{"user", nil, map[string]any{"user": "ali@firma.com"}, "ali@firma.com"},
		{"safety_identifier before user", nil, map[string]any{"safety_identifier": "s-1", "user": "u-1"}, "s-1"},
		{"header only is not sent", map[string]string{"X-Ragmux-End-User": "hdr-1"}, nil, ""},
		{"header and body: the body's value goes", map[string]string{"X-Ragmux-End-User": "hdr-1"}, map[string]any{"user": "u-1"}, "u-1"},
		{"invalid user is dropped", nil, map[string]any{"user": "a b"}, ""},
		{"user over 512 characters does not break the request", nil, map[string]any{"user": long}, ""},
		{"invalid safety_identifier does not fall through", nil, map[string]any{"safety_identifier": "a b", "user": "u-1"}, ""},
		{"nothing", nil, nil, ""},
	}
	for _, tc := range cases {
		e, up := anthropicChatEnv(t, nil)
		body := map[string]any{"messages": userMsg}
		for k, v := range tc.extra {
			body[k] = v
		}
		resp, out := e.chatWith(tc.headers, body)
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d %v", tc.name, resp.StatusCode, out)
		}
		b := up.lastBody(t)
		raw, _ := json.Marshal(b)
		if tc.want == "" {
			if _, has := b["metadata"]; has {
				t.Errorf("%s: the upstream saw metadata %v, want none", tc.name, b["metadata"])
			}
		} else {
			md, _ := b["metadata"].(map[string]any)
			id, _ := md["user_id"].(string)
			if id != hashedEndUser(tc.want) || len(id) != 63 || id != strings.ToLower(id) {
				t.Errorf("%s: metadata.user_id = %q, want %q", tc.name, id, hashedEndUser(tc.want))
			}
		}
		// No raw value of any kind is on the wire.
		for _, leak := range []string{"ali@firma.com", "hdr-1", "u-1", "s-1", "a b", long} {
			if strings.Contains(string(raw), leak) {
				t.Errorf("%s: %q reached the Anthropic upstream: %.200s", tc.name, leak, raw)
			}
		}
		if _, has := b["user"]; has {
			t.Errorf("%s: the upstream saw a user field", tc.name)
		}
		if _, has := b["safety_identifier"]; has {
			t.Errorf("%s: the upstream saw a safety_identifier field", tc.name)
		}
	}
}

func TestAnthropicTranslationAlsoSendsItForAStream(t *testing.T) {
	e, up := anthropicChatEnv(t, nil)
	resp := e.post(context.Background(), "/v1/chat/completions",
		map[string]any{"messages": userMsg, "stream": true, "user": "ali@firma.com"}, e.key)
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	md, _ := up.lastBody(t)["metadata"].(map[string]any)
	if md["user_id"] != hashedEndUser("ali@firma.com") {
		t.Errorf("metadata = %v", md)
	}
}

func TestEmbeddingsEndUserIsRecordedAndNotForwarded(t *testing.T) {
	e := newEnv(t, nil)
	up := newScripted(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":[{"index":0,"embedding":[1,0,0,0]}],"usage":{"prompt_tokens":3,"total_tokens":3}}`)
	})
	conn, err := e.st.CreateConnection(context.Background(), &store.ModelConnection{Name: "emb", ProviderType: "custom_openai",
		BaseURL: up.srv.URL + "/v1", APIKey: "sk-secretsecretsecret123", ModelName: "emb-model"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.CreateRoute(context.Background(), &store.Route{ProjectID: e.proj.ID, Name: "emb",
		Kind: store.RouteKindEmbeddings, Strategy: store.RouteStrategyWeighted,
		Targets: []store.RouteTarget{{ConnectionID: conn.ID, Weight: 1}}}); err != nil {
		t.Fatal(err)
	}

	resp, out := e.call(http.MethodPost, "/v1/embeddings", map[string]any{"model": "emb", "input": "hello", "user": "emb-user", "safety_identifier": "emb-safety"}, e.key, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %v", resp.StatusCode, out)
	}
	if rec := e.lastLog(); rec.EndUser != "emb-safety" || rec.Endpoint != "embeddings" {
		t.Errorf("row = end_user %q endpoint %q, want emb-safety (safety_identifier first)", rec.EndUser, rec.Endpoint)
	}
	raw, _ := json.Marshal(up.lastBody(t))
	for _, leak := range []string{"emb-user", "emb-safety", "user", "safety_identifier"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("%q reached the embedding upstream: %s", leak, raw)
		}
	}

	// The header wins; the body's fields of the wrong type do not break the
	// request, as they never did.
	resp, out = e.call(http.MethodPost, "/v1/embeddings", map[string]any{"model": "emb", "input": "hello", "user": 7}, e.key,
		map[string]string{"X-Ragmux-End-User": "emb-hdr"})
	if resp.StatusCode != 200 {
		t.Fatalf("%d %v", resp.StatusCode, out)
	}
	if rec := e.lastLog(); rec.EndUser != "emb-hdr" {
		t.Errorf("end_user = %q, want emb-hdr", rec.EndUser)
	}
	if !strings.Contains(warningsOf(resp), "user is not a usable end user id") {
		t.Errorf("Warning = %v", resp.Header.Values("Warning"))
	}

	// An invalid header is the same 400 as on chat.
	resp, out = e.call(http.MethodPost, "/v1/embeddings", map[string]any{"model": "emb", "input": "hello"}, e.key, map[string]string{"X-Ragmux-End-User": "no good"})
	if resp.StatusCode != http.StatusBadRequest || errorField(t, out, "code") != "invalid_end_user" {
		t.Errorf("%d %v", resp.StatusCode, out)
	}
}

func TestEndUserAndTagsHeadersOnRetrieveAndRerank(t *testing.T) {
	e := newEnv(t, nil)
	e.attachRAG()
	hdr := map[string]string{"X-Ragmux-End-User": "ret-user", "X-Ragmux-Tags": "support,eu"}
	resp, out := e.call(http.MethodPost, "/v1/retrieve", map[string]any{"query": "apples"}, e.key, hdr)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %v", resp.StatusCode, out)
	}
	rec := e.lastLog()
	if rec.Endpoint != "retrieve" || rec.EndUser != "ret-user" || !reflect.DeepEqual(rec.Tags, []string{"eu", "support"}) {
		t.Errorf("retrieve row = endpoint %q end_user %q tags %v", rec.Endpoint, rec.EndUser, rec.Tags)
	}
	// A body field is not a source on this endpoint: only the header is.
	resp, out = e.call(http.MethodPost, "/v1/retrieve", map[string]any{"query": "apples", "user": "body-user"}, e.key, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %v", resp.StatusCode, out)
	}
	if rec := e.lastLog(); rec.EndUser != "" {
		t.Errorf("end_user = %q, want none", rec.EndUser)
	}
	resp, out = e.call(http.MethodPost, "/v1/retrieve", map[string]any{"query": "apples"}, e.key, map[string]string{"X-Ragmux-End-User": "bad one"})
	if resp.StatusCode != http.StatusBadRequest || errorField(t, out, "code") != "invalid_end_user" {
		t.Errorf("%d %v", resp.StatusCode, out)
	}
	resp, out = e.call(http.MethodPost, "/v1/rerank", map[string]any{"query": "q", "documents": []string{"a"}}, e.key, map[string]string{"X-Ragmux-Tags": "bad tag"})
	if resp.StatusCode != http.StatusBadRequest || errorField(t, out, "code") != "invalid_tags" {
		t.Errorf("%d %v", resp.StatusCode, out)
	}
}

// The MCP search is a pipeline endpoint too: the headers are read from the MCP
// request, a bad one is the tool's own error, and the row carries both.
func TestEndUserAndTagsOnTheMCPSearch(t *testing.T) {
	e := newEnv(t, nil)
	e.attachRAG()
	type answer struct {
		Result  *MCPSearchResult
		Failure *MCPFailure
	}
	var last answer
	handler := e.gw.MCPAuth(func(w http.ResponseWriter, status int, code, msg string) { w.WriteHeader(status) })(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			last.Result, last.Failure = e.gw.MCPSearch(w, r, MCPSearchArgs{Query: "apples"})
		}))
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	search := func(h map[string]string) answer {
		last = answer{}
		req, _ := http.NewRequest(http.MethodPost, srv.URL, nil)
		req.Header.Set("Authorization", "Bearer "+e.key)
		for k, v := range h {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return last
	}

	if got := search(map[string]string{"X-Ragmux-End-User": "mcp-user", "X-Ragmux-Tags": "agent,mcp"}); got.Failure != nil || got.Result == nil {
		t.Fatalf("a search: %+v", got.Failure)
	}
	if rec := e.lastLog(); rec.Endpoint != "retrieve" || rec.EndUser != "mcp-user" || !reflect.DeepEqual(rec.Tags, []string{"agent", "mcp"}) {
		t.Errorf("row = endpoint %q end_user %q tags %v", rec.Endpoint, rec.EndUser, rec.Tags)
	}
	if got := search(map[string]string{"X-Ragmux-End-User": "no good"}); got.Failure == nil || got.Failure.Status != http.StatusBadRequest || got.Failure.Code != "invalid_end_user" {
		t.Errorf("a bad end user: %+v", got.Failure)
	}
	if got := search(map[string]string{"X-Ragmux-Tags": "a,,b"}); got.Failure == nil || got.Failure.Status != http.StatusBadRequest || got.Failure.Code != "invalid_tags" {
		t.Errorf("bad tags: %+v", got.Failure)
	}
}

func TestTags(t *testing.T) {
	e := newEnv(t, nil)
	seen := e.recordUpstream(nil)

	// The header: comma separated, spaces around the commas dropped, sorted and
	// de-duplicated.
	resp, out := e.chatWith(map[string]string{"X-Ragmux-Tags": "beta, alpha ,beta,v1.2:x/y_z-0"}, map[string]any{"messages": userMsg})
	if resp.StatusCode != 200 {
		t.Fatalf("%d %v", resp.StatusCode, out)
	}
	if rec := e.lastLog(); !reflect.DeepEqual(rec.Tags, []string{"alpha", "beta", "v1.2:x/y_z-0"}) {
		t.Errorf("tags = %v", rec.Tags)
	}

	// The extension, which the upstream never sees; no "unknown field" warning.
	resp, out = e.chat(map[string]any{"messages": userMsg, "ragmux": map[string]any{"tags": []string{"z", "y", "z"}}})
	if resp.StatusCode != 200 {
		t.Fatalf("%d %v", resp.StatusCode, out)
	}
	if rec := e.lastLog(); !reflect.DeepEqual(rec.Tags, []string{"y", "z"}) {
		t.Errorf("tags = %v", rec.Tags)
	}
	if _, has := seen.last()["ragmux"]; has {
		t.Error("the ragmux object reached the upstream")
	}
	if len(resp.Header.Values("Warning")) != 0 {
		t.Errorf("Warning = %v", resp.Header.Values("Warning"))
	}

	// Both: the extension wins and a Warning says the header was not used.
	resp, out = e.chatWith(map[string]string{"X-Ragmux-Tags": "from-header"},
		map[string]any{"messages": userMsg, "ragmux": map[string]any{"tags": []string{"from-body"}}})
	if resp.StatusCode != 200 {
		t.Fatalf("%d %v", resp.StatusCode, out)
	}
	if rec := e.lastLog(); !reflect.DeepEqual(rec.Tags, []string{"from-body"}) {
		t.Errorf("tags = %v, want the extension's", rec.Tags)
	}
	if !strings.Contains(warningsOf(resp), "X-Ragmux-Tags is ignored") {
		t.Errorf("Warning = %v", resp.Header.Values("Warning"))
	}

	// At most eight, counted after de-duplication.
	eight := "t1,t2,t3,t4,t5,t6,t7,t8,t1,t2"
	if resp, out := e.chatWith(map[string]string{"X-Ragmux-Tags": eight}, map[string]any{"messages": userMsg}); resp.StatusCode != 200 {
		t.Errorf("eight distinct tags: %d %v", resp.StatusCode, out)
	}
	if rec := e.lastLog(); len(rec.Tags) != 8 {
		t.Errorf("tags = %v, want eight", rec.Tags)
	}

	// No tags, no tags column.
	if resp, _ := e.chat(map[string]any{"messages": userMsg}); resp.StatusCode != 200 {
		t.Fatal("plain chat failed")
	}
	if rec := e.lastLog(); len(rec.Tags) != 0 {
		t.Errorf("tags = %v, want none", rec.Tags)
	}
}

func TestInvalidTagsAre400(t *testing.T) {
	e := newEnv(t, nil)
	seen := e.recordUpstream(nil)
	long := strings.Repeat("t", 65)
	headers := map[string]string{
		"space":        "a b",
		"empty item":   "a,,b",
		"trailing":     "a,",
		"empty header": "",
		"nine":         "t1,t2,t3,t4,t5,t6,t7,t8,t9",
		"too long":     long,
		"semicolon":    "a;b",
		"quote":        `a"b`,
		"non ascii":    "ş",
	}
	for name, v := range headers {
		resp, out := e.chatWith(map[string]string{"X-Ragmux-Tags": v}, map[string]any{"messages": userMsg})
		if resp.StatusCode != http.StatusBadRequest || errorField(t, out, "code") != "invalid_tags" {
			t.Errorf("header %s: %d %v, want 400 invalid_tags", name, resp.StatusCode, out)
		}
	}
	bodies := map[string]any{
		"a string":     "x",
		"not strings":  []any{1, 2},
		"null":         nil,
		"space":        []string{"a b"},
		"empty string": []string{""},
		"nine":         []string{"t1", "t2", "t3", "t4", "t5", "t6", "t7", "t8", "t9"},
		"too long":     []string{long},
		"an object":    map[string]any{"a": 1},
	}
	for name, v := range bodies {
		resp, out := e.chat(map[string]any{"messages": userMsg, "ragmux": map[string]any{"tags": v}})
		if resp.StatusCode != http.StatusBadRequest || errorField(t, out, "code") != "invalid_tags" {
			t.Errorf("ragmux.tags %s: %d %v, want 400 invalid_tags", name, resp.StatusCode, out)
		}
	}
	// A bad extension is refused even when the header is fine, and the other
	// way round: neither is quietly dropped.
	resp, out := e.chatWith(map[string]string{"X-Ragmux-Tags": "good"}, map[string]any{"messages": userMsg, "ragmux": map[string]any{"tags": []string{"bad tag"}}})
	if resp.StatusCode != http.StatusBadRequest || errorField(t, out, "code") != "invalid_tags" {
		t.Errorf("%d %v", resp.StatusCode, out)
	}
	if seen.count() != 0 {
		t.Errorf("a refused request reached the upstream %d times", seen.count())
	}
	if rows, err := e.st.RecentRequests(context.Background(), store.MetricsFilter{ProjectID: &e.proj.ID}, 10); err != nil || len(rows) != 0 {
		t.Errorf("refused requests wrote %d rows (%v)", len(rows), err)
	}
	// The ragmux object's other errors keep their own code.
	resp, out = e.chat(map[string]any{"messages": userMsg, "ragmux": "nope"})
	if resp.StatusCode != http.StatusBadRequest || errorField(t, out, "code") != "invalid_ragmux" {
		t.Errorf("%d %v", resp.StatusCode, out)
	}
}

func TestTagsAndEndUserOnMessages(t *testing.T) {
	e := newEnv(t, nil)
	body := func(tags any) map[string]any {
		return map[string]any{"model": "claude-x", "max_tokens": 32, "messages": []any{map[string]any{"role": "user", "content": "hi"}},
			"ragmux": map[string]any{"tags": tags}}
	}
	resp, raw := e.messages(body([]string{"b", "a"}), map[string]string{"x-api-key": e.key, "X-Ragmux-Tags": "hdr"})
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	if rec := e.lastLog(); !reflect.DeepEqual(rec.Tags, []string{"a", "b"}) {
		t.Errorf("tags = %v, want the extension's", rec.Tags)
	}
	if !strings.Contains(warningsOf(resp), "X-Ragmux-Tags is ignored") {
		t.Errorf("Warning = %v", resp.Header.Values("Warning"))
	}
	resp, raw = e.messages(body([]string{"bad tag"}), map[string]string{"x-api-key": e.key})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	if _, code, _ := envelopeOf(t, raw); code != "invalid_tags" {
		t.Errorf("code = %q: %s", code, raw)
	}
	resp, raw = e.messages(messagesBody, map[string]string{"x-api-key": e.key, "X-Ragmux-End-User": "bad one"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	if _, code, _ := envelopeOf(t, raw); code != "invalid_end_user" {
		t.Errorf("code = %q: %s", code, raw)
	}
}

func TestEndUserCeilingAnswers429(t *testing.T) {
	e := newEnv(t, func(p *store.Project) { p.EndUserDailyTokens = 10 })
	hdr := map[string]string{"X-Ragmux-End-User": "heavy"}

	// Each request spends 5+1 tokens: the second is served (6 < 10) and takes
	// the end user to 12.
	for i := 1; i <= 2; i++ {
		resp, out := e.chatWith(hdr, map[string]any{"messages": userMsg})
		if resp.StatusCode != 200 {
			t.Fatalf("request %d: %d %v", i, resp.StatusCode, out)
		}
		e.waitEndUser("heavy", i)
	}
	resp, out := e.chatWith(hdr, map[string]any{"messages": userMsg})
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("over the ceiling: %d %v, want 429", resp.StatusCode, out)
	}
	if errorField(t, out, "code") != "budget_daily" || errorField(t, out, "scope") != "end_user" ||
		errorField(t, out, "type") != "insufficient_quota" {
		t.Errorf("error = %v", out["error"])
	}
	if msg, _ := errorField(t, out, "message").(string); !strings.HasPrefix(msg, "Daily token budget exhausted (12 of 10 tokens used).") {
		t.Errorf("message = %q", msg)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("a daily ceiling resets: Retry-After is missing")
	}
	// The denial is a 429 row of that end user, and spends nothing.
	rows, _ := e.st.RecentRequests(context.Background(), store.MetricsFilter{ProjectID: &e.proj.ID}, 10)
	var denied *store.RequestLog
	for _, r := range rows {
		if r.StatusCode == http.StatusTooManyRequests {
			denied = r
		}
	}
	if denied == nil || denied.Error != "budget_daily" || denied.EndUser != "heavy" {
		t.Errorf("denial row = %+v", denied)
	}
	if u := e.waitEndUser("heavy", 2); u.Requests != 2 {
		t.Errorf("end user requests = %d, want 2", u.Requests)
	}

	// Another end user, and a request that names none, are not held to it.
	if resp, out := e.chatWith(map[string]string{"X-Ragmux-End-User": "light"}, map[string]any{"messages": userMsg}); resp.StatusCode != 200 {
		t.Errorf("another end user: %d %v", resp.StatusCode, out)
	}
	for i := 0; i < 3; i++ {
		if resp, out := e.chat(map[string]any{"messages": userMsg}); resp.StatusCode != 200 {
			t.Errorf("no end user, request %d: %d %v", i+1, resp.StatusCode, out)
		}
	}
	// The body's own field names the end user too.
	if resp, _ := e.chat(map[string]any{"messages": userMsg, "user": "heavy"}); resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("the body's user was not held to the ceiling: %d", resp.StatusCode)
	}
}

func TestEndUserRequestsPerMinuteReservesAndReleases(t *testing.T) {
	e := newEnv(t, func(p *store.Project) { p.EndUserRPM = 2; p.RateLimitRPM = 100 })
	hdr := map[string]string{"X-Ragmux-End-User": "bursty"}
	for i := 1; i <= 2; i++ {
		resp, out := e.chatWith(hdr, map[string]any{"messages": userMsg})
		if resp.StatusCode != 200 {
			t.Fatalf("request %d: %d %v", i, resp.StatusCode, out)
		}
		if got := resp.Header.Get("x-ratelimit-limit-requests"); got != "2" {
			t.Errorf("x-ratelimit-limit-requests = %q, want the end user's 2 (tighter than the project's 100)", got)
		}
	}
	resp, out := e.chatWith(hdr, map[string]any{"messages": userMsg})
	if resp.StatusCode != http.StatusTooManyRequests || errorField(t, out, "code") != "rate_limit_rpm" || errorField(t, out, "scope") != "end_user" {
		t.Fatalf("third request: %d %v", resp.StatusCode, out)
	}
	if msg, _ := errorField(t, out, "message").(string); !strings.Contains(msg, "2 requests per minute for this end user") {
		t.Errorf("message = %q", msg)
	}
	// The slot the denied request took was released from both tiers.
	u, err := e.gw.Limiter.Usage(context.Background(), e.proj)
	if err != nil || u.Minute.Requests != 2 {
		t.Errorf("project minute requests = %+v (%v), want 2", u.Minute, err)
	}
}

func TestProjectWithoutAnEndUserCeilingWritesNoCounters(t *testing.T) {
	e := newEnv(t, nil)
	hdr := map[string]string{"X-Ragmux-End-User": "someone"}
	for i := 0; i < 2; i++ {
		if resp, out := e.chatWith(hdr, map[string]any{"messages": userMsg}); resp.StatusCode != 200 {
			t.Fatalf("%d %v", resp.StatusCode, out)
		}
	}
	rec := e.lastLog()
	if rec.EndUser != "someone" {
		t.Fatalf("end_user = %q", rec.EndUser)
	}
	// The project counters are written by record just before the row's reader
	// can see it; give the deferred write its moment.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if u, err := e.gw.Limiter.Usage(context.Background(), e.proj); err == nil && u.Day.Requests >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n := e.endUserRows(); n != 0 {
		t.Errorf("end_user_usage has %d rows, want none: the project sets no ceiling", n)
	}

	// With a ceiling set, the same requests are counted.
	e.proj.EndUserMonthlyTokens = 1_000_000
	if _, err := e.st.UpdateProject(context.Background(), e.proj); err != nil {
		t.Fatal(err)
	}
	if resp, out := e.chatWith(hdr, map[string]any{"messages": userMsg}); resp.StatusCode != 200 {
		t.Fatalf("%d %v", resp.StatusCode, out)
	}
	e.waitEndUser("someone", 1)
	// A request without an end user writes nothing even then.
	before := e.endUserRows()
	if resp, _ := e.chat(map[string]any{"messages": userMsg}); resp.StatusCode != 200 {
		t.Fatal("plain chat failed")
	}
	time.Sleep(200 * time.Millisecond)
	if after := e.endUserRows(); after != before {
		t.Errorf("a request without an end user changed end_user_usage rows: %d -> %d", before, after)
	}
}

// The end user and the tags are row columns and nothing else: never a metric
// label, never a span attribute (ADR-002, PRD rule 10).
func TestEndUserAndTagsAreNotMetricLabelsOrSpanAttributes(t *testing.T) {
	e := newEnv(t, func(p *store.Project) { p.EndUserDailyTokens = 1_000_000 })
	reg := e.withMetrics()
	col := tracingtest.NewCollector(t)
	tr := tracing.New(tracing.Config{Endpoint: col.URL(), Protocol: tracing.ProtocolGRPC, SampleRatio: 1, ServiceName: "t"})
	e.gw.Tracer = tr

	hdr := map[string]string{"X-Ragmux-End-User": "never-a-label", "X-Ragmux-Tags": "never-a-tag"}
	if resp, out := e.chatWith(hdr, map[string]any{"messages": userMsg, "user": "never-a-body-label"}); resp.StatusCode != 200 {
		t.Fatalf("%d %v", resp.StatusCode, out)
	}
	if rec := e.lastLog(); rec.EndUser != "never-a-label" || !reflect.DeepEqual(rec.Tags, []string{"never-a-tag"}) {
		t.Fatalf("row = end_user %q tags %v", rec.EndUser, rec.Tags)
	}
	e.waitEndUser("never-a-label", 1)

	// A denial by the end-user tier counts the limits_denied series with its
	// closed reason, and nothing of the end user.
	e.proj.EndUserDailyTokens = 1
	if _, err := e.st.UpdateProject(context.Background(), e.proj); err != nil {
		t.Fatal(err)
	}
	if resp, _ := e.chatWith(hdr, map[string]any{"messages": userMsg}); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatal("the second request should be over the ceiling")
	}

	text := reg.Text()
	for _, leak := range []string{"never-a-label", "never-a-tag", "never-a-body-label", "end_user", "tag="} {
		if strings.Contains(text, leak) {
			t.Errorf("%q reached the metrics output", leak)
		}
	}
	mustContain(t, text, `ragmux_limits_denied_total{project="`+strconv.FormatInt(e.proj.ID, 10)+`",reason="budget_daily"} 1`)

	// The request span ends just after the handler returns; give it a moment,
	// then flush the exporter.
	time.Sleep(300 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := tr.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	spans := col.Spans(t)
	if len(spans) == 0 {
		t.Fatal("the collector received no spans")
	}
	for _, sp := range spans {
		enc, _ := json.Marshal(sp)
		for _, leak := range []string{"never-a-label", "never-a-tag", "never-a-body-label", "end_user"} {
			if strings.Contains(string(enc), leak) {
				t.Errorf("%q reached span %q: %s", leak, sp.Name, enc)
			}
		}
	}
}

// The bounds are defined where they are enforced (provider, store); the texts
// that tell a client about them are built from the same constants, and the two
// definitions of the end user's length must not drift apart.
func TestEndUserBoundsAndTheirTextsAgree(t *testing.T) {
	if provider.MaxEndUserBytes != store.MaxEndUserBytes {
		t.Fatalf("provider.MaxEndUserBytes = %d, store.MaxEndUserBytes = %d", provider.MaxEndUserBytes, store.MaxEndUserBytes)
	}
	if !strings.Contains(endUserValueRule, strconv.Itoa(store.MaxEndUserBytes)) {
		t.Errorf("end user rule %q does not state the limit %d", endUserValueRule, store.MaxEndUserBytes)
	}
	_, err := checkTags(make([]string, 0))
	if err != nil {
		t.Fatal(err)
	}
	tooMany := make([]string, store.MaxTags+1)
	for i := range tooMany {
		tooMany[i] = "t" + strconv.Itoa(i)
	}
	if _, err := checkTags(tooMany); err == nil || !strings.Contains(err.Error(), strconv.Itoa(store.MaxTags)) {
		t.Errorf("too many tags: %v, want a message naming %d", err, store.MaxTags)
	}
	if _, err := checkTags([]string{"bad tag"}); err == nil || !strings.Contains(err.Error(), strconv.Itoa(store.MaxTagBytes)) {
		t.Errorf("bad tag: %v, want a message naming %d", err, store.MaxTagBytes)
	}
	if !validTag(strings.Repeat("t", store.MaxTagBytes)) || validTag(strings.Repeat("t", store.MaxTagBytes+1)) {
		t.Error("validTag does not follow store.MaxTagBytes")
	}
}

func TestEndUserUnitHelpers(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"a", true}, {"ali@firma.com", true}, {"A1._:@+=/-", true}, {strings.Repeat("a", 128), true},
		{"", false}, {strings.Repeat("a", 129), false}, {"a b", false}, {"a\tb", false}, {`a"b`, false}, {"a'b", false},
		{"a,b", false}, {"a;b", false}, {"ş", false}, {"a\x00b", false}, {"a\nb", false},
	} {
		if got := provider.ValidEndUser(tc.in); got != tc.want {
			t.Errorf("valid(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"a", true}, {"v1.2:x/y_z-0", true}, {strings.Repeat("t", 64), true},
		{"", false}, {strings.Repeat("t", 65), false}, {"a b", false}, {"a@b", false}, {"a+b", false}, {"a=b", false}, {"ş", false},
	} {
		if got := validTag(tc.in); got != tc.want {
			t.Errorf("validTag(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
