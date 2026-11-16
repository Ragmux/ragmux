package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ragmux/ragmux/internal/guardrail"
	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/internal/webhook"
)

// Guardrails (ADR-034): the stages, the actions, the endpoints they cover and
// what is and is not recorded. The rule engine itself is tested in
// internal/guardrail; these tests are about the gateway's use of it.

const (
	// tckn and iban are valid identifiers (their checksums hold) that belong to
	// nobody; the tests plant them where a real one could turn up.
	tckn = "10000000146"
	iban = "TR33 0006 1005 1978 6457 8413 26"
)

// guardRule is a rule of the stored configuration.
func guardRule(id, typ, action string, stages ...string) guardrail.Rule {
	return guardrail.Rule{ID: id, Type: typ, Action: action, Stages: stages}
}

// guard stores the project's guardrails; the gateway reads the project on every
// request, so it takes effect at once.
func (e *env) guard(cfg guardrail.Config) {
	e.t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.st.SetProjectGuardrails(context.Background(), e.proj.ID, raw); err != nil {
		e.t.Fatal(err)
	}
}

// guardRules is guard for a plain list of rules.
func (e *env) guardRules(rules ...guardrail.Rule) { e.guard(guardrail.Config{Rules: rules}) }

// guardRow is what the guardrail columns of a request log row hold.
type guardRow struct {
	Status    int
	Stage     string
	Redacted  int
	Flags     int
	Rules     []string
	Webhook   string
	SkipCause string
	Error     string
}

// guardRowOf waits for the row of a request and reads the guardrail columns,
// which the admin API does not expose yet.
func (e *env) guardRowOf(resp *http.Response) guardRow {
	e.t.Helper()
	id := resp.Header.Get("X-Request-Id")
	if id == "" {
		e.t.Fatal("the response carries no X-Request-Id")
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var r guardRow
		var stage, webhook, skip, errText *string
		err := e.st.DB().QueryRow(context.Background(), `SELECT status_code, guardrail_blocked_stage, guardrail_redactions,
			guardrail_flags, guardrail_rules, guardrail_webhook, rag_skip_reason, error FROM request_logs
			WHERE request_id = $1 ORDER BY id DESC LIMIT 1`, id).
			Scan(&r.Status, &stage, &r.Redacted, &r.Flags, &r.Rules, &webhook, &skip, &errText)
		if err == nil {
			r.Stage, r.Webhook, r.SkipCause, r.Error = deref(stage), deref(webhook), deref(skip), deref(errText)
			return r
		}
		time.Sleep(20 * time.Millisecond)
	}
	e.t.Fatalf("no request log row for %s", id)
	return guardRow{}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// chatCalls counts the chat calls the upstream received.
func (e *env) chatCalls() (*seenBodies, func() int) {
	seen := e.recordUpstream(nil)
	return seen, seen.count
}

// errOf decodes the OpenAI error object of a response body.
func errOf(t *testing.T, out map[string]any) (typ, code, msg string) {
	t.Helper()
	e, ok := out["error"].(map[string]any)
	if !ok {
		t.Fatalf("no error object in %v", out)
	}
	typ, _ = e["type"].(string)
	code, _ = e["code"].(string)
	msg, _ = e["message"].(string)
	return typ, code, msg
}

func userBody(text string) map[string]any {
	return map[string]any{"messages": []map[string]any{{"role": "user", "content": text}}}
}

// upstreamText is the text of the last user message the upstream received.
func lastUserText(b map[string]any) string {
	msgs, _ := b["messages"].([]any)
	for i := len(msgs) - 1; i >= 0; i-- {
		m, _ := msgs[i].(map[string]any)
		if m["role"] == "user" {
			if s, ok := m["content"].(string); ok {
				return s
			}
		}
	}
	return ""
}

// ---------------------------------------------------------------- pre_request

func TestPreRequestBlock(t *testing.T) {
	e := newEnv(t, nil)
	reg := e.withMetrics()
	seen, calls := e.chatCalls()
	e.guardRules(guardRule("kimlik", guardrail.TypeTCKN, guardrail.ActionBlock, guardrail.StagePreRequest))

	resp, out := e.chat(userBody("TC no: " + tckn))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d: %v", resp.StatusCode, out)
	}
	typ, code, msg := errOf(t, out)
	if typ != "invalid_request_error" || code != "guardrail_blocked" || msg != "request blocked by project guardrail (stage: pre_request)" {
		t.Errorf("error = %q %q %q", typ, code, msg)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "kimlik") || strings.Contains(string(raw), tckn) {
		t.Errorf("the answer names the rule or echoes the match: %s", raw)
	}
	if calls() != 0 {
		t.Error("a blocked request reached the upstream")
	}
	row := e.guardRowOf(resp)
	if row.Status != 400 || row.Stage != "pre_request" || strings.Join(row.Rules, ",") != "kimlik" || row.Error != "guardrail_blocked" {
		t.Errorf("row = %+v", row)
	}
	mustContain(t, reg.Text(), fmt.Sprintf(`ragmux_guardrail_events_total{project="%d",stage="pre_request",action="block"} 1`, e.proj.ID))
	_ = seen

	// Nothing to block: the request goes through.
	if resp, _ := e.chat(userBody("hello")); resp.StatusCode != http.StatusOK {
		t.Errorf("an innocent request: %d", resp.StatusCode)
	}
}

// A blocked request is a request: it takes a slot of the request limit (the
// webhook call of a limitless client must not be free, and the block itself is
// not a way to probe without cost).
func TestPreRequestBlockCountsAgainstTheRequestLimit(t *testing.T) {
	e := newEnv(t, func(p *store.Project) { p.RateLimitRPM = 2 })
	e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionBlock, guardrail.StagePreRequest))
	for i := 0; i < 2; i++ {
		if resp, _ := e.chat(userBody(tckn)); resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("blocked request %d: %d", i+1, resp.StatusCode)
		}
	}
	resp, out := e.chat(userBody("hello"))
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("the third request: %d %v, want 429 after two blocked ones used the limit", resp.StatusCode, out)
	}
}

// The texts the stage reads, and the ones it does not.
func TestPreRequestReadsEverySurfaceOfTheRequest(t *testing.T) {
	msg := func(role string, content any) map[string]any { return map[string]any{"role": role, "content": content} }
	call := func(args string) map[string]any {
		return map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{
			"id": "c1", "type": "function", "function": map[string]any{"name": "lookup", "arguments": args}}}}
	}
	blocked := map[string][]map[string]any{
		"a string":            {msg("user", "id "+tckn)},
		"a text part":         {msg("user", []any{map[string]any{"type": "text", "text": "id " + tckn}})},
		"a system message":    {msg("system", "id "+tckn), msg("user", "hi")},
		"an earlier turn":     {msg("user", "id "+tckn), msg("assistant", "ok"), msg("user", "thanks")},
		"a tool result":       {msg("user", "hi"), call(`{}`), {"role": "tool", "tool_call_id": "c1", "content": "id " + tckn}},
		"tool call arguments": {msg("user", "hi"), call(`{"id":"` + tckn + `"}`)},
		"the assistant":       {msg("user", "hi"), msg("assistant", "id "+tckn), msg("user", "and?")},
	}
	for name, msgs := range blocked {
		e := newEnv(t, nil)
		e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionBlock, guardrail.StagePreRequest))
		if resp, out := e.chat(map[string]any{"messages": msgs}); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: %d %v, want 400", name, resp.StatusCode, out)
		}
	}

	// Not read: an image, the project's own prompt, a tool definition.
	e := newEnv(t, func(p *store.Project) { p.SystemPrompt = "Operator note, id " + tckn })
	_, calls := e.chatCalls()
	e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionBlock, guardrail.StagePreRequest))
	resp, out := e.chat(map[string]any{
		"messages": []any{msg("user", []any{
			map[string]any{"type": "text", "text": "what is this"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://img.example/" + tckn + ".png"}}})},
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "lookup",
			"description": "finds " + tckn, "parameters": map[string]any{"type": "object"}}}},
	})
	if resp.StatusCode != http.StatusOK || calls() != 1 {
		t.Errorf("an image URL, a tool definition and the project prompt are not read: %d %v", resp.StatusCode, out)
	}
}

func TestPreRequestRedact(t *testing.T) {
	e := newEnv(t, func(p *store.Project) { p.SystemPrompt = "Operator note, id " + tckn })
	reg := e.withMetrics()
	seen := e.recordUpstream(nil)
	e.guardRules(guardRule("kimlik", guardrail.TypeTCKN, guardrail.ActionRedact, guardrail.StagePreRequest),
		guardRule("hesap", guardrail.TypeIBAN, guardrail.ActionRedact, guardrail.StagePreRequest))

	body := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": "id " + tckn + " and again " + tckn},
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{
			"id": "c1", "type": "function", "function": map[string]any{"name": "pay", "arguments": `{"to":"` + iban + `"}`}}}},
		map[string]any{"role": "tool", "tool_call_id": "c1", "content": "sent to " + iban},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "last: " + tckn},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://img.example/a.png"}}}},
	}}
	resp, out := e.chat(body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %v", resp.StatusCode, out)
	}
	if w := resp.Header.Values("Warning"); len(w) != 1 || w[0] != `299 ragmux "guardrail redacted 5 match(es)"` {
		t.Errorf("Warning = %q", w)
	}
	sent, _ := json.Marshal(seen.last())
	text := string(sent)
	clientPart := text[strings.Index(text, `{"content":"id [REDACTED`):]
	for _, leak := range []string{tckn, "TR33", "6457 8413"} {
		if strings.Contains(clientPart, leak) {
			t.Errorf("%q reached the upstream: %s", leak, text)
		}
	}
	// The operator's prompt is the operator's: it is injected after the stage
	// and keeps what it says.
	if !strings.Contains(text, "Operator note, id "+tckn) {
		t.Errorf("the project's own prompt was changed: %s", text)
	}
	mustContain(t, text, "id [REDACTED:kimlik] and again [REDACTED:kimlik]", `[REDACTED:hesap]`, "last: [REDACTED:kimlik]",
		"https://img.example/a.png", `"name":"pay"`)
	row := e.guardRowOf(resp)
	if row.Status != 200 || row.Redacted != 5 || row.Stage != "" || strings.Join(row.Rules, ",") != "kimlik,hesap" {
		t.Errorf("row = %+v", row)
	}
	mustContain(t, reg.Text(), fmt.Sprintf(`ragmux_guardrail_events_total{project="%d",stage="pre_request",action="redact"} 5`, e.proj.ID))
}

func TestPreRequestFlagChangesNothingAndSaysNothing(t *testing.T) {
	e := newEnv(t, nil)
	reg := e.withMetrics()
	seen := e.recordUpstream(nil)
	e.guardRules(guardRule("f", guardrail.TypeTCKN, guardrail.ActionFlag, guardrail.StagePreRequest))
	resp, out := e.chat(userBody("id " + tckn))
	if resp.StatusCode != http.StatusOK || len(resp.Header.Values("Warning")) != 0 {
		t.Fatalf("status %d warnings %v %v", resp.StatusCode, resp.Header.Values("Warning"), out)
	}
	if got := lastUserText(seen.last()); got != "id "+tckn {
		t.Errorf("the upstream got %q: a flag must not change the request", got)
	}
	if row := e.guardRowOf(resp); row.Flags != 1 || row.Redacted != 0 || row.Stage != "" || strings.Join(row.Rules, ",") != "f" {
		t.Errorf("row = %+v", row)
	}
	mustContain(t, reg.Text(), fmt.Sprintf(`ragmux_guardrail_events_total{project="%d",stage="pre_request",action="flag"} 1`, e.proj.ID))
}

// A project whose stored configuration this build cannot use is refused, not
// served unguarded.
func TestUnusableConfigurationFailsClosed(t *testing.T) {
	e := newEnv(t, nil)
	_, calls := e.chatCalls()
	if err := e.st.SetProjectGuardrails(context.Background(), e.proj.ID,
		json.RawMessage(`{"rules":[{"id":"x","type":"from-the-future","action":"block","stages":["pre_request"]}]}`)); err != nil {
		t.Fatal(err)
	}
	resp, out := e.chat(userBody("hello"))
	typ, code, _ := errOf(t, out)
	if resp.StatusCode != http.StatusServiceUnavailable || typ != "server_error" || code != "guardrail_unavailable" || calls() != 0 {
		t.Errorf("status %d %v", resp.StatusCode, out)
	}
	if resp.Header.Get("Retry-After") != "" {
		t.Error("an outage of the guardrails is not a retry-later answer")
	}
	if row := e.guardRowOf(resp); row.Status != 503 {
		t.Errorf("row = %+v", row)
	}
}

// ------------------------------------------------------------- pre_retrieval

func TestPreRetrievalChat(t *testing.T) {
	setup := func(t *testing.T, rule guardrail.Rule) *env {
		e := newEnv(t, nil)
		e.attachRAG()
		e.up.resetEvents()
		e.guardRules(rule)
		return e
	}
	query := userBody("tell me about apples " + tckn)

	t.Run("block skips retrieval and the request goes on", func(t *testing.T) {
		e := setup(t, guardRule("k", guardrail.TypeTCKN, guardrail.ActionBlock, guardrail.StagePreRetrieval))
		reg := e.withMetrics()
		base := e.up.embeds.Load()
		resp, out := e.chat(query)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d: %v", resp.StatusCode, out)
		}
		if resp.Header.Get("x-ragmux-rag-skip") != "guardrail" || resp.Header.Get("x-ragmux-rag-hits") != "0" || e.up.embeds.Load() != base {
			t.Errorf("skip %q hits %q embeds %d", resp.Header.Get("x-ragmux-rag-skip"), resp.Header.Get("x-ragmux-rag-hits"), e.up.embeds.Load()-base)
		}
		// The request was served: the block is recorded as the stage that
		// refused something, with the status of the answer.
		row := e.guardRowOf(resp)
		if row.Status != 200 || row.SkipCause != "guardrail" || row.Stage != "pre_retrieval" {
			t.Errorf("row = %+v", row)
		}
		mustContain(t, reg.Text(), fmt.Sprintf(`ragmux_rag_skipped_total{project="%d",reason="guardrail"} 1`, e.proj.ID),
			fmt.Sprintf(`ragmux_guardrail_events_total{project="%d",stage="pre_retrieval",action="block"} 1`, e.proj.ID))
	})
	t.Run("redact searches with the redacted query", func(t *testing.T) {
		e := setup(t, guardRule("k", guardrail.TypeTCKN, guardrail.ActionRedact, guardrail.StagePreRetrieval))
		seen := e.recordUpstream(nil)
		resp, _ := e.chat(query)
		got := e.up.seen()
		if resp.StatusCode != http.StatusOK || len(got) == 0 || strings.Contains(got[0], tckn) || !strings.Contains(got[0], "[REDACTED:k]") {
			t.Errorf("status %d, embedding events %q", resp.StatusCode, got)
		}
		// The model still gets what the client wrote: only the search changed.
		if text := lastUserText(seen.last()); !strings.Contains(text, tckn) {
			t.Errorf("the upstream got %q, want the client's own text", text)
		}
		if w := resp.Header.Values("Warning"); len(w) != 1 || !strings.Contains(w[0], "retrieval query") {
			t.Errorf("Warning = %q", w)
		}
		if row := e.guardRowOf(resp); row.Redacted != 1 || row.SkipCause != "" {
			t.Errorf("row = %+v", row)
		}
	})
	t.Run("flag counts", func(t *testing.T) {
		e := setup(t, guardRule("k", guardrail.TypeTCKN, guardrail.ActionFlag, guardrail.StagePreRetrieval))
		resp, _ := e.chat(query)
		if got := e.up.seen(); resp.StatusCode != http.StatusOK || len(got) == 0 || !strings.Contains(got[0], tckn) {
			t.Errorf("status %d, embedding events %q", resp.StatusCode, got)
		}
		if row := e.guardRowOf(resp); row.Flags != 1 {
			t.Errorf("row = %+v", row)
		}
	})
}

// The query is read after the condense call has rewritten it: a model that
// writes a TCKN into the query is caught too.
func TestPreRetrievalReadsTheCondensedQuery(t *testing.T) {
	e := condenseEnv(t, nil)
	e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionBlock, guardrail.StagePreRetrieval))
	e.up.scriptCondense(condenseReply("the plan of "+tckn, 40, 6), nil)
	resp, out := e.chat(map[string]any{"messages": followUp})
	if resp.StatusCode != http.StatusOK || resp.Header.Get("x-ragmux-rag-skip") != "guardrail" {
		t.Fatalf("status %d skip %q %v", resp.StatusCode, resp.Header.Get("x-ragmux-rag-skip"), out)
	}
	if got := e.up.seen(); fmt.Sprint(got) != "[chat:condense chat:answer]" {
		t.Errorf("upstream events = %q: the condense call ran, the search did not", got)
	}
	// The condense call's tokens are still charged.
	if rec := e.lastLog(); rec.PromptTokens != 5+40 {
		t.Errorf("prompt tokens = %d, want the condense call's 40 on top of the answer's 5", rec.PromptTokens)
	}
}

func TestRetrieveEndpoint(t *testing.T) {
	post := func(e *env, q string) (*http.Response, map[string]any) {
		return e.call(http.MethodPost, "/v1/retrieve", map[string]any{"query": q}, e.key, nil)
	}
	setup := func(t *testing.T, action string) *env {
		e := newEnv(t, nil)
		e.attachRAG()
		e.up.resetEvents()
		e.guardRules(guardRule("k", guardrail.TypeTCKN, action, guardrail.StagePreRetrieval))
		return e
	}
	t.Run("block is a 400", func(t *testing.T) {
		e := setup(t, guardrail.ActionBlock)
		base := e.up.embeds.Load()
		resp, out := post(e, "apples "+tckn)
		typ, code, msg := errOf(t, out)
		if resp.StatusCode != http.StatusBadRequest || typ != "invalid_request_error" || code != "guardrail_blocked" ||
			msg != "request blocked by project guardrail (stage: pre_retrieval)" {
			t.Fatalf("status %d: %v", resp.StatusCode, out)
		}
		if e.up.embeds.Load() != base {
			t.Error("a blocked query was embedded")
		}
		if row := e.guardRowOf(resp); row.Status != 400 || row.Stage != "pre_retrieval" {
			t.Errorf("row = %+v", row)
		}
	})
	t.Run("redact searches the redacted query", func(t *testing.T) {
		e := setup(t, guardrail.ActionRedact)
		resp, out := post(e, "apples "+tckn)
		if resp.StatusCode != http.StatusOK || out["hits"] == nil {
			t.Fatalf("status %d: %v", resp.StatusCode, out)
		}
		if got := e.up.seen(); len(got) != 1 || strings.Contains(got[0], tckn) || !strings.Contains(got[0], "[REDACTED:k]") {
			t.Errorf("embedding events %q", got)
		}
		if w := resp.Header.Values("Warning"); len(w) != 1 || !strings.Contains(w[0], "redacted 1 match(es)") {
			t.Errorf("Warning = %q", w)
		}
	})
	t.Run("flag counts", func(t *testing.T) {
		e := setup(t, guardrail.ActionFlag)
		resp, _ := post(e, "apples "+tckn)
		if row := e.guardRowOf(resp); resp.StatusCode != http.StatusOK || row.Flags != 1 {
			t.Errorf("status %d row %+v", resp.StatusCode, row)
		}
	})
	t.Run("a pre_request rule does not apply to a retrieval", func(t *testing.T) {
		e := newEnv(t, nil)
		e.attachRAG()
		e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionBlock, guardrail.StagePreRequest))
		if resp, out := post(e, "apples "+tckn); resp.StatusCode != http.StatusOK {
			t.Errorf("status %d: %v", resp.StatusCode, out)
		}
	})
}

// The MCP search tool goes through the same stage; a block is the tool's own
// error, not a refusal of the HTTP request.
func TestMCPSearchIsGuarded(t *testing.T) {
	e := newEnv(t, nil)
	e.attachRAG()
	e.up.resetEvents()
	e.guardRules(guardRule("k", guardrail.TypeTCKN, guardrail.ActionBlock, guardrail.StagePreRetrieval))

	type answer struct {
		Result  *MCPSearchResult
		Failure *MCPFailure
	}
	var last answer
	handler := e.gw.MCPAuth(func(w http.ResponseWriter, status int, code, msg string) { w.WriteHeader(status) })(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			last.Result, last.Failure = e.gw.MCPSearch(w, r, MCPSearchArgs{Query: r.URL.Query().Get("q")})
		}))
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	search := func(q string) answer {
		last = answer{}
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/?q="+url.QueryEscape(q), nil)
		req.Header.Set("Authorization", "Bearer "+e.key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return last
	}

	base := e.up.embeds.Load()
	got := search("apples " + tckn)
	if got.Failure == nil || !got.Failure.Tool || got.Failure.Code != "guardrail_blocked" || got.Failure.Status != 400 {
		t.Fatalf("a blocked query: %+v", got.Failure)
	}
	if e.up.embeds.Load() != base {
		t.Error("a blocked query was embedded")
	}
	if got := search("apples"); got.Failure != nil || got.Result == nil || len(got.Result.Hits) == 0 {
		t.Errorf("an innocent query: %+v", got.Failure)
	}
}

// ------------------------------------------------------------------- webhook

// hook is a scripted guardrail webhook receiver.
type hook struct {
	srv   *httptest.Server
	mu    sync.Mutex
	calls []hookCall
	reply atomic.Pointer[http.HandlerFunc]
}

type hookCall struct {
	Header http.Header
	Body   map[string]any
	Raw    []byte
}

func (h *hook) set(fn http.HandlerFunc) { h.reply.Store(&fn) }

func (h *hook) seen() []hookCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]hookCall(nil), h.calls...)
}

// answer is a handler that replies with a JSON body.
func answer(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}
}

// newHook starts a receiver on loopback that allows everything, registers it as
// the project's guardrail endpoint and gives the gateway a client that may
// reach it.
func (e *env) newHook() *hook {
	e.t.Helper()
	h := &hook{}
	h.set(answer(`{"action":"allow"}`))
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(raw))
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		h.mu.Lock()
		h.calls = append(h.calls, hookCall{Header: r.Header.Clone(), Body: body, Raw: raw})
		h.mu.Unlock()
		(*h.reply.Load())(w, r)
	}))
	e.t.Cleanup(h.srv.Close)
	e.gw.Webhooks = webhook.NewClient(map[string]bool{"127.0.0.1": true})
	e.registerHook(h.srv.URL)
	return h
}

func (e *env) registerHook(url string) {
	e.t.Helper()
	if _, err := e.st.CreateWebhookEndpoint(context.Background(), &store.WebhookEndpoint{ProjectID: e.proj.ID,
		Purpose: store.WebhookPurposeGuardrail, URL: url, Secret: "whsec_" + strings.Repeat("QUJD", 11)}); err != nil {
		e.t.Fatal(err)
	}
}

// withHook is the project's configuration for a webhook and no local rule.
func (e *env) withHook(timeoutMS int, mode string, rules ...guardrail.Rule) {
	e.guard(guardrail.Config{Rules: rules, Webhook: &guardrail.Webhook{Enabled: true, TimeoutMS: timeoutMS, FailureMode: mode}})
}

func TestWebhookRequestShape(t *testing.T) {
	e := newEnv(t, nil)
	h := e.newHook()
	e.withHook(2000, "", guardRule("kimlik", guardrail.TypeTCKN, guardrail.ActionRedact, guardrail.StagePreRequest))
	resp, out := e.chat(map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": "be brief"},
		map[string]any{"role": "user", "content": "id " + tckn + " please"}}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %v", resp.StatusCode, out)
	}
	calls := h.seen()
	if len(calls) != 1 {
		t.Fatalf("%d webhook calls, want 1", len(calls))
	}
	c := calls[0]
	if c.Body["version"] != "2026-11-01" || c.Body["stage"] != "pre_request" || c.Body["endpoint"] != "chat.completions" ||
		c.Body["project_id"] != float64(e.proj.ID) || c.Body["request_id"] != resp.Header.Get("X-Request-Id") {
		t.Errorf("body = %s", c.Raw)
	}
	texts, _ := c.Body["texts"].([]any)
	if len(texts) != 2 {
		t.Fatalf("texts = %v", texts)
	}
	first, second := texts[0].(map[string]any), texts[1].(map[string]any)
	if first["index"] != float64(0) || first["role"] != "system" || first["text"] != "be brief" ||
		second["index"] != float64(1) || second["role"] != "user" {
		t.Errorf("texts = %v", texts)
	}
	// The local rules run first: the receiver gets the text already redacted.
	if second["text"] != "id [REDACTED:kimlik] please" || strings.Contains(string(c.Raw), tckn) {
		t.Errorf("the webhook was sent the unredacted text: %s", c.Raw)
	}
	if c.Header.Get("webhook-signature") == "" || c.Header.Get("webhook-timestamp") == "" || !strings.HasPrefix(c.Header.Get("webhook-id"), "evt_") {
		t.Errorf("headers = %v", c.Header)
	}
	if row := e.guardRowOf(resp); row.Webhook != "ok" || row.Redacted != 1 {
		t.Errorf("row = %+v", row)
	}
}

func TestWebhookVerdicts(t *testing.T) {
	cases := map[string]struct {
		reply  string
		status int
		upText string // what the upstream should see as the user's text
		row    func(guardRow) bool
	}{
		"allow": {`{"action":"allow"}`, 200, "my iban " + iban, func(r guardRow) bool { return r.Rules == nil && r.Webhook == "ok" }},
		"flag":  {`{"action":"flag","reason":"pii"}`, 200, "my iban " + iban, func(r guardRow) bool { return r.Flags == 1 && strings.Join(r.Rules, ",") == "webhook:pii" }},
		"redact": {`{"action":"redact","reason":"pii","texts":[{"index":0,"text":"my iban [X]"}]}`, 200, "my iban [X]",
			func(r guardRow) bool { return r.Redacted == 1 && strings.Join(r.Rules, ",") == "webhook:pii" }},
		"redact that changes nothing": {`{"action":"redact","texts":[{"index":0,"text":"my iban ` + iban + `"}]}`, 200, "my iban " + iban,
			func(r guardRow) bool { return r.Redacted == 0 }},
		"block": {`{"action":"block","reason":"policy.7"}`, 400, "", func(r guardRow) bool {
			return r.Stage == "pre_request" && strings.Join(r.Rules, ",") == "webhook:policy.7"
		}},
		"texts are ignored unless redacting": {`{"action":"allow","texts":[{"index":0,"text":"changed"}]}`, 200, "my iban " + iban, func(guardRow) bool { return true }},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, nil)
			seen := e.recordUpstream(nil)
			h := e.newHook()
			h.set(answer(tc.reply))
			e.withHook(2000, "")
			resp, out := e.chat(userBody("my iban " + iban))
			if resp.StatusCode != tc.status {
				t.Fatalf("status %d: %v", resp.StatusCode, out)
			}
			if tc.status == 400 {
				if _, code, _ := errOf(t, out); code != "guardrail_blocked" || seen.count() != 0 {
					t.Errorf("code %q, upstream calls %d", code, seen.count())
				}
			} else if got := lastUserText(seen.last()); got != tc.upText {
				t.Errorf("the upstream got %q, want %q", got, tc.upText)
			}
			if row := e.guardRowOf(resp); !tc.row(row) {
				t.Errorf("row = %+v", row)
			}
		})
	}
}

func TestWebhookFailureModes(t *testing.T) {
	failures := map[string]struct {
		setup   func(h *hook)
		outcome string
	}{
		"a 500": {func(h *hook) { h.set(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }) }, "error"},
		"a 404": {func(h *hook) { h.set(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(404) }) }, "error"},
		"a redirect": {func(h *hook) {
			h.set(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/elsewhere", http.StatusFound) })
		}, "error"},
		"not JSON":          {func(h *hook) { h.set(answer(`allow`)) }, "error"},
		"an unknown action": {func(h *hook) { h.set(answer(`{"action":"shrug"}`)) }, "error"},
		"no action":         {func(h *hook) { h.set(answer(`{}`)) }, "error"},
		"a bad reason":      {func(h *hook) { h.set(answer(`{"action":"flag","reason":"Not Allowed!"}`)) }, "error"},
		"too few texts":     {func(h *hook) { h.set(answer(`{"action":"redact","texts":[]}`)) }, "error"},
		"too many texts": {func(h *hook) {
			h.set(answer(`{"action":"redact","texts":[{"index":0,"text":"a"},{"index":1,"text":"b"}]}`))
		}, "error"},
		"texts out of order": {func(h *hook) { h.set(answer(`{"action":"redact","texts":[{"index":1,"text":"a"}]}`)) }, "error"},
		"a text missing":     {func(h *hook) { h.set(answer(`{"action":"redact","texts":[{"index":0}]}`)) }, "error"},
		"a timeout": {func(h *hook) {
			h.set(func(w http.ResponseWriter, r *http.Request) {
				select {
				case <-time.After(3 * time.Second):
				case <-r.Context().Done():
				}
			})
		}, "timeout"},
	}
	for name, tc := range failures {
		t.Run(name+", fail closed", func(t *testing.T) {
			e := newEnv(t, nil)
			_, calls := e.chatCalls()
			reg := e.withMetrics()
			h := e.newHook()
			tc.setup(h)
			e.withHook(150, "") // the default mode is fail_closed
			resp, out := e.chat(userBody("hello"))
			typ, code, _ := errOf(t, out)
			if resp.StatusCode != http.StatusServiceUnavailable || typ != "server_error" || code != "guardrail_unavailable" {
				t.Fatalf("status %d: %v", resp.StatusCode, out)
			}
			if resp.Header.Get("Retry-After") != "" || calls() != 0 {
				t.Errorf("Retry-After %q, upstream calls %d", resp.Header.Get("Retry-After"), calls())
			}
			if len(h.seen()) != 1 {
				t.Errorf("%d webhook calls: a failed call is not retried", len(h.seen()))
			}
			row := e.guardRowOf(resp)
			if row.Status != 503 || row.Webhook != tc.outcome || row.Stage != "" {
				t.Errorf("row = %+v", row)
			}
			mustContain(t, reg.Text(), fmt.Sprintf(`ragmux_guardrail_webhook_total{project="%d",outcome="%s"} 1`, e.proj.ID, tc.outcome))
		})
		t.Run(name+", fail open", func(t *testing.T) {
			e := newEnv(t, nil)
			_, calls := e.chatCalls()
			reg := e.withMetrics()
			h := e.newHook()
			tc.setup(h)
			e.withHook(150, guardrail.FailOpen)
			resp, out := e.chat(userBody("hello"))
			if resp.StatusCode != http.StatusOK || calls() != 1 {
				t.Fatalf("status %d upstream calls %d: %v", resp.StatusCode, calls(), out)
			}
			if row := e.guardRowOf(resp); row.Webhook != "skipped_fail_open" {
				t.Errorf("row = %+v", row)
			}
			mustContain(t, reg.Text(), fmt.Sprintf(`ragmux_guardrail_webhook_total{project="%d",outcome="skipped_fail_open"} 1`, e.proj.ID))
		})
	}
}

func TestWebhookUnreachable(t *testing.T) {
	for _, mode := range []string{"", guardrail.FailOpen} {
		e := newEnv(t, nil)
		dead := httptest.NewServer(http.NotFoundHandler())
		dead.Close() // nothing listens there any more
		e.gw.Webhooks = webhook.NewClient(map[string]bool{"127.0.0.1": true})
		e.registerHook(dead.URL)
		e.withHook(500, mode)
		resp, _ := e.chat(userBody("hello"))
		want := http.StatusServiceUnavailable
		if mode == guardrail.FailOpen {
			want = http.StatusOK
		}
		if resp.StatusCode != want {
			t.Errorf("mode %q: status %d, want %d", mode, resp.StatusCode, want)
		}
	}
}

// Everything that would stand between the gateway and a receiver is a failure
// of the webhook, and the project's failure mode decides.
func TestWebhookCannotBeCalled(t *testing.T) {
	t.Run("no client in the gateway", func(t *testing.T) {
		e := newEnv(t, nil)
		e.registerHook("https://hooks.example.com/guard")
		e.gw.Webhooks = nil
		e.withHook(500, "")
		if resp, _ := e.chat(userBody("hello")); resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("status %d: a fail-closed project must not be served when nothing can ask the webhook", resp.StatusCode)
		}
	})
	t.Run("no endpoint registered", func(t *testing.T) {
		e := newEnv(t, nil)
		e.gw.Webhooks = webhook.NewClient(nil)
		e.withHook(500, "")
		if resp, _ := e.chat(userBody("hello")); resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("status %d", resp.StatusCode)
		}
	})
	t.Run("endpoint disabled", func(t *testing.T) {
		e := newEnv(t, nil)
		h := e.newHook()
		ep, err := e.st.GetWebhookEndpoint(context.Background(), e.proj.ID, store.WebhookPurposeGuardrail)
		if err != nil {
			t.Fatal(err)
		}
		if err := e.st.DisableWebhookEndpoint(context.Background(), ep.ID); err != nil {
			t.Fatal(err)
		}
		e.withHook(500, "")
		if resp, _ := e.chat(userBody("hello")); resp.StatusCode != http.StatusServiceUnavailable || len(h.seen()) != 0 {
			t.Errorf("status %d, %d calls", resp.StatusCode, len(h.seen()))
		}
	})
	t.Run("a host the client may not reach", func(t *testing.T) {
		e := newEnv(t, nil)
		h := e.newHook()
		e.gw.Webhooks = webhook.NewClient(nil) // loopback is not on the allowlist
		e.withHook(500, "")
		if resp, _ := e.chat(userBody("hello")); resp.StatusCode != http.StatusServiceUnavailable || len(h.seen()) != 0 {
			t.Errorf("status %d, %d calls", resp.StatusCode, len(h.seen()))
		}
	})
	t.Run("a request over 1 MB is not sent", func(t *testing.T) {
		e := newEnv(t, nil)
		h := e.newHook()
		e.withHook(500, "")
		resp, _ := e.chat(userBody(strings.Repeat("a", 1100<<10)))
		if resp.StatusCode != http.StatusServiceUnavailable || len(h.seen()) != 0 {
			t.Errorf("status %d, %d calls: the body is over the limit and counts as a failure", resp.StatusCode, len(h.seen()))
		}
		if row := e.guardRowOf(resp); row.Webhook != "error" {
			t.Errorf("row = %+v", row)
		}
	})
}

func TestWebhookStages(t *testing.T) {
	// The webhook sees the request and the retrieval query, never the answer.
	e := newEnv(t, nil)
	e.attachRAG()
	h := e.newHook()
	e.withHook(2000, "", guardRule("a", guardrail.TypeTCKN, guardrail.ActionFlag, guardrail.StagePostResponse))
	e.up.setChat(func(w http.ResponseWriter, r *http.Request) { okChat(w, r) })
	if resp, out := e.chat(userBody("tell me about apples")); resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %v", resp.StatusCode, out)
	}
	var stages []string
	for _, c := range h.seen() {
		stages = append(stages, c.Body["stage"].(string))
		if c.Body["stage"] == "pre_retrieval" {
			texts := c.Body["texts"].([]any)
			if len(texts) != 1 || texts[0].(map[string]any)["role"] != "query" || texts[0].(map[string]any)["text"] != "tell me about apples" {
				t.Errorf("retrieval texts = %v", texts)
			}
		}
	}
	if strings.Join(stages, ",") != "pre_request,pre_retrieval" {
		t.Errorf("webhook stages = %v, want pre_request then pre_retrieval and nothing after the answer", stages)
	}

	// A blocked query skips the search but not the request; a webhook that
	// cannot be asked about the query fails closed.
	h.set(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if strings.Contains(string(raw), `"stage":"pre_retrieval"`) {
			answer(`{"action":"block","reason":"q"}`)(w, r)
			return
		}
		answer(`{"action":"allow"}`)(w, r)
	})
	e.up.resetEvents()
	base := e.up.embeds.Load()
	resp, _ := e.chat(userBody("tell me about apples"))
	if resp.StatusCode != http.StatusOK || resp.Header.Get("x-ragmux-rag-skip") != "guardrail" || e.up.embeds.Load() != base {
		t.Errorf("a webhook block at pre_retrieval: status %d skip %q embeds %d", resp.StatusCode, resp.Header.Get("x-ragmux-rag-skip"), e.up.embeds.Load()-base)
	}
	h.set(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if strings.Contains(string(raw), `"stage":"pre_retrieval"`) {
			w.WriteHeader(500)
			return
		}
		answer(`{"action":"allow"}`)(w, r)
	})
	if resp, _ := e.chat(userBody("tell me about apples")); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("a webhook outage at pre_retrieval: status %d, want 503", resp.StatusCode)
	}
}
