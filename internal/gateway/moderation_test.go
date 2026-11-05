package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/ragmux/ragmux/internal/guardrail"
	"github.com/ragmux/ragmux/internal/store"
)

// POST /v1/moderations (ADR-034): the project's pre_request rules, reported and
// never applied.

type moderated struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Results []struct {
		Flagged        bool               `json:"flagged"`
		Categories     map[string]bool    `json:"categories"`
		CategoryScores map[string]float64 `json:"category_scores"`
		Ragmux         struct {
			Action string `json:"action"`
		} `json:"ragmux"`
	} `json:"results"`
}

func (e *env) moderate(input any) (*http.Response, moderated, map[string]any) {
	e.t.Helper()
	resp, raw := e.call(http.MethodPost, "/v1/moderations", map[string]any{"input": input, "model": "omni-moderation-latest"}, e.key, nil)
	var m moderated
	b, _ := json.Marshal(raw)
	_ = json.Unmarshal(b, &m)
	return resp, m, raw
}

func TestModerationShape(t *testing.T) {
	e := newEnv(t, nil)
	seen, calls := e.chatCalls()
	e.guardRules(
		guardRule("kimlik", guardrail.TypeTCKN, guardrail.ActionBlock, guardrail.StagePreRequest),
		guardRule("hesap", guardrail.TypeIBAN, guardrail.ActionRedact, guardrail.StagePreRequest),
		guardRule("not-here", guardrail.TypeTCKN, guardrail.ActionFlag, guardrail.StagePreRequest),
		guardRule("answer-only", guardrail.TypeTCKN, guardrail.ActionBlock, guardrail.StagePostResponse),
	)
	resp, got, raw := e.moderate([]string{"id " + tckn, "iban " + iban, "clean"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %v", resp.StatusCode, raw)
	}
	if !regexp.MustCompile(`^modr-[0-9a-f]{24}$`).MatchString(got.ID) || got.Model != "ragmux-guardrails" || len(got.Results) != 3 {
		t.Fatalf("response = %+v", got)
	}
	r0, r1, r2 := got.Results[0], got.Results[1], got.Results[2]
	// Every pre_request rule is a category, matched or not; other stages' rules
	// are not.
	for i, r := range got.Results {
		if len(r.Categories) != 3 || len(r.CategoryScores) != 3 {
			t.Errorf("result %d: categories %v scores %v", i, r.Categories, r.CategoryScores)
		}
		if _, has := r.Categories["answer-only"]; has {
			t.Errorf("result %d lists a post_response rule", i)
		}
	}
	if !r0.Flagged || !r0.Categories["kimlik"] || r0.Categories["hesap"] || r0.CategoryScores["kimlik"] != 1 || r0.Ragmux.Action != "block" {
		t.Errorf("result 0 = %+v", r0)
	}
	// "not-here" is the same checksum as "kimlik" -- it matches too, as a flag.
	if !r0.Categories["not-here"] {
		t.Errorf("result 0 = %+v", r0)
	}
	if !r1.Flagged || !r1.Categories["hesap"] || r1.Ragmux.Action != "redact" {
		t.Errorf("result 1 = %+v", r1)
	}
	if r2.Flagged || r2.Ragmux.Action != "none" || r2.CategoryScores["kimlik"] != 0 {
		t.Errorf("result 2 = %+v", r2)
	}
	// A single string is an input too.
	if _, one, _ := e.moderate("clean"); len(one.Results) != 1 {
		t.Errorf("one string: %+v", one)
	}
	// Nothing is applied: no upstream, no model, no tokens.
	if calls() != 0 || seen.count() != 0 {
		t.Error("a moderation reached the model")
	}
	if resp.Header.Get("Warning") != "" {
		t.Errorf("Warning = %q", resp.Header.Get("Warning"))
	}
}

func TestModerationWithoutRules(t *testing.T) {
	e := newEnv(t, nil)
	resp, got, raw := e.moderate("anything " + tckn)
	if resp.StatusCode != http.StatusOK || len(got.Results) != 1 || got.Results[0].Flagged || got.Results[0].Ragmux.Action != "none" ||
		got.Results[0].Categories == nil || len(got.Results[0].Categories) != 0 {
		t.Errorf("%d %v", resp.StatusCode, raw)
	}
	// The categories are an empty object, never null.
	b, _ := json.Marshal(raw)
	if !strings.Contains(string(b), `"categories":{}`) || !strings.Contains(string(b), `"category_scores":{}`) {
		t.Errorf("%s", b)
	}
}

func TestModerationRow(t *testing.T) {
	e := newEnv(t, nil)
	reg := e.withMetrics()
	e.guardRules(guardRule("kimlik", guardrail.TypeTCKN, guardrail.ActionBlock, guardrail.StagePreRequest))
	resp, _, _ := e.moderate("id " + tckn)
	row := e.guardRowOf(resp)
	// A moderation is a request: a row with its endpoint, the rules that
	// matched, the status of the answer (a block rule only reports) and no
	// tokens.
	if row.Status != 200 || strings.Join(row.Rules, ",") != "kimlik" || row.Stage != "" {
		t.Errorf("row = %+v", row)
	}
	var endpoint string
	var prompt, completion int
	if err := e.st.DB().QueryRow(context.Background(), `SELECT endpoint, prompt_tokens, completion_tokens FROM request_logs
		WHERE request_id = $1`, resp.Header.Get("X-Request-Id")).Scan(&endpoint, &prompt, &completion); err != nil {
		t.Fatal(err)
	}
	if endpoint != "moderations" || prompt != 0 || completion != 0 {
		t.Errorf("endpoint %q tokens %d/%d", endpoint, prompt, completion)
	}
	mustContain(t, reg.Text(), fmt.Sprintf(`ragmux_gateway_requests_total{project="%d",model="ragmux-guardrails",status="200",streamed="false"} 1`, e.proj.ID))
	if strings.Contains(reg.Text(), "ragmux_guardrail_events_total{") {
		t.Error("a report counted as an applied action")
	}
}

func TestModerationLimitsAndScope(t *testing.T) {
	t.Run("a request of the request limit", func(t *testing.T) {
		e := newEnv(t, func(p *store.Project) { p.RateLimitRPM = 1 })
		if resp, _, raw := e.moderate("one"); resp.StatusCode != 200 {
			t.Fatalf("%d %v", resp.StatusCode, raw)
		}
		if resp, _, _ := e.moderate("two"); resp.StatusCode != http.StatusTooManyRequests {
			t.Errorf("the second call: %d, want 429", resp.StatusCode)
		}
	})
	t.Run("input limits", func(t *testing.T) {
		e := newEnv(t, nil)
		many := make([]string, 33)
		for name, in := range map[string]any{"missing": nil, "empty list": []string{}, "33 inputs": many, "a number": 7,
			"an object": map[string]any{"a": 1}, "a list of numbers": []int{1}} {
			resp, _, raw := e.moderate(in)
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("%s: %d %v", name, resp.StatusCode, raw)
			}
		}
		many = many[:32]
		if resp, got, _ := e.moderate(many); resp.StatusCode != 200 || len(got.Results) != 32 {
			t.Errorf("32 inputs: %d %d", resp.StatusCode, len(got.Results))
		}
		bad := e.post(context.Background(), "/v1/moderations", "{not json", e.key)
		_ = bad.Body.Close()
		if bad.StatusCode != http.StatusBadRequest {
			t.Errorf("malformed body: %d", bad.StatusCode)
		}
	})
	t.Run("a key that cannot chat", func(t *testing.T) {
		e := newEnv(t, nil)
		_, narrow := e.userKey("embed-only", []int64{e.proj.ID}, func(k *store.APIKey) { k.Scopes = []string{store.ScopeEmbeddings} })
		if resp, _ := e.call(http.MethodPost, "/v1/moderations", map[string]any{"input": "x"}, narrow, nil); resp.StatusCode != http.StatusForbidden {
			t.Errorf("status %d, want 403", resp.StatusCode)
		}
		if resp, _ := e.call(http.MethodPost, "/v1/moderations", map[string]any{"input": "x"}, "", nil); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("no key: %d", resp.StatusCode)
		}
	})
	t.Run("an unusable configuration", func(t *testing.T) {
		e := newEnv(t, nil)
		if err := e.st.SetProjectGuardrails(context.Background(), e.proj.ID, json.RawMessage(`{"rules":[{"id":"x","type":"?","action":"block","stages":["pre_request"]}]}`)); err != nil {
			t.Fatal(err)
		}
		resp, _, raw := e.moderate("x")
		if _, code, _ := errOf(t, raw); resp.StatusCode != http.StatusServiceUnavailable || code != "guardrail_unavailable" {
			t.Errorf("%d %v", resp.StatusCode, raw)
		}
	})
}

func TestModerationWebhook(t *testing.T) {
	t.Run("a flag marks every input, nothing is applied", func(t *testing.T) {
		e := newEnv(t, nil)
		h := e.newHook()
		h.set(answer(`{"action":"flag","reason":"odd"}`))
		e.withHook(2000, "")
		resp, got, _ := e.moderate([]string{"a", "b"})
		if resp.StatusCode != 200 || len(got.Results) != 2 {
			t.Fatalf("status %d", resp.StatusCode)
		}
		for i, r := range got.Results {
			if !r.Flagged || !r.Categories["webhook"] || r.Ragmux.Action != "flag" {
				t.Errorf("result %d = %+v", i, r)
			}
		}
		calls := h.seen()
		if len(calls) != 1 || calls[0].Body["endpoint"] != "moderations" || calls[0].Body["stage"] != "pre_request" || len(calls[0].Body["texts"].([]any)) != 2 {
			t.Errorf("webhook calls = %+v", calls)
		}
		if row := e.guardRowOf(resp); strings.Join(row.Rules, ",") != "webhook:odd" || row.Webhook != "ok" {
			t.Errorf("row = %+v", row)
		}
	})
	t.Run("a redact marks the inputs it changed", func(t *testing.T) {
		e := newEnv(t, nil)
		h := e.newHook()
		h.set(answer(`{"action":"redact","texts":[{"index":0,"text":"a"},{"index":1,"text":"[X]"}]}`))
		e.withHook(2000, "")
		_, got, _ := e.moderate([]string{"a", "b"})
		if got.Results[0].Flagged || !got.Results[1].Flagged || got.Results[1].Ragmux.Action != "redact" {
			t.Errorf("results = %+v", got.Results)
		}
	})
	t.Run("an allow adds nothing", func(t *testing.T) {
		e := newEnv(t, nil)
		e.newHook()
		e.withHook(2000, "")
		_, got, _ := e.moderate("a")
		if got.Results[0].Flagged || got.Results[0].Ragmux.Action != "none" {
			t.Errorf("results = %+v", got.Results)
		}
	})
	t.Run("a failure follows the failure mode", func(t *testing.T) {
		for mode, want := range map[string]int{"": http.StatusServiceUnavailable, guardrail.FailOpen: http.StatusOK} {
			e := newEnv(t, nil)
			h := e.newHook()
			h.set(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) })
			e.withHook(500, mode)
			resp, _, raw := e.moderate("a")
			if resp.StatusCode != want {
				t.Errorf("mode %q: %d %v, want %d", mode, resp.StatusCode, raw, want)
			}
			if want == http.StatusServiceUnavailable {
				if _, code, _ := errOf(t, raw); code != "guardrail_unavailable" || resp.Header.Get("Retry-After") != "" {
					t.Errorf("mode %q: code %q Retry-After %q", mode, code, resp.Header.Get("Retry-After"))
				}
			}
		}
	})
}
