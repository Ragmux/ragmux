package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// The signature and the redacted payload in testdata/anthropic_thinking.*.
// They carry "+", "/" and "=" on purpose: a base64 value is the first thing a
// careless re-encoding damages.
const (
	fixtureThinkingSignature = "EuYBCkYIDBgCKkB7Qm1v3fJ0yR8nA4sL2pT6uW9eZ5cH3kD1gV7oN0qYSWgHR7YpuX2q9eL4mC8vB1nK6sT3wJ0aF5dG7hE2iU9oP4rQ1yZ8xC3vN6bM0lK2jH5gF8dS1a+W4eR7tY0uI3oP6/Zq9=="
	fixtureRedactedData      = "EmwKAhgBEgy3va3pzix/LafPsn4aDFIT2Xlxh0L5L8rLVyIwxtE3rAFBa8cr3qpPkNRj2YYDeXYZ2Js6vb3x+Zq/0r="
)

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func hasWarning(list []string, substr string) bool {
	for _, s := range list {
		if strings.Contains(s, substr) {
			return true
		}
	}
	return false
}

func warningsOf(t *testing.T, cfg Config, reqJSON string) []string {
	t.Helper()
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rw, ok := p.(RequestWarner)
	if !ok {
		t.Fatalf("%s adapter has no RequestWarnings", cfg.ProviderType)
	}
	return rw.RequestWarnings(parseReq(t, reqJSON))
}

func asProviderError(t *testing.T, err error) *Error {
	t.Helper()
	pe, ok := err.(*Error)
	if !ok {
		t.Fatalf("error = %v, want *Error", err)
	}
	return pe
}

// ------------------------------------------------------------------ effort

func TestAnthropicEffortAdaptiveSendsEffortNotBudget(t *testing.T) {
	req := parseReq(t, `{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`)
	got, err := translateAnthropic(context.Background(), Config{Model: "claude-opus-4-7"}, req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(got)
	var wire map[string]json.RawMessage
	_ = json.Unmarshal(raw, &wire)
	if string(wire["thinking"]) != `{"type":"adaptive"}` {
		t.Errorf("thinking = %s", wire["thinking"])
	}
	if string(wire["output_config"]) != `{"effort":"high"}` {
		t.Errorf("output_config = %s", wire["output_config"])
	}
	if strings.Contains(string(raw), "budget_tokens") {
		t.Errorf("adaptive model got budget_tokens: %s", raw)
	}
	// The client set no max_tokens, and 4096 could all go to thinking.
	if got.MaxTokens != 16000 {
		t.Errorf("max_tokens = %d, want 16000", got.MaxTokens)
	}
}

func TestAnthropicEffortBudgetSendsBudgetTokens(t *testing.T) {
	req := parseReq(t, `{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`)
	got, err := translateAnthropic(context.Background(), Config{Model: "claude-sonnet-4-5"}, req)
	if err != nil {
		t.Fatal(err)
	}
	if got.Thinking == nil || got.Thinking.Type != "enabled" || got.Thinking.BudgetTokens != 16384 {
		t.Fatalf("thinking = %+v", got.Thinking)
	}
	if got.OutputConfig != nil {
		t.Errorf("sonnet 4.5 takes no effort, got %+v", got.OutputConfig)
	}
	if got.MaxTokens != 16384+4096 {
		t.Errorf("max_tokens = %d, want %d", got.MaxTokens, 16384+4096)
	}
	raw, _ := json.Marshal(got)
	if !strings.Contains(string(raw), `"thinking":{"type":"enabled","budget_tokens":16384}`) {
		t.Errorf("wire = %s", raw)
	}
}

func TestAnthropicEffortOpus45SendsBoth(t *testing.T) {
	req := parseReq(t, `{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"xhigh"}`)
	got, err := translateAnthropic(context.Background(), Config{Model: "claude-opus-4-5"}, req)
	if err != nil {
		t.Fatal(err)
	}
	if got.Thinking == nil || got.Thinking.BudgetTokens != 32768 {
		t.Errorf("thinking = %+v", got.Thinking)
	}
	if got.OutputConfig == nil || got.OutputConfig.Effort != "high" {
		t.Errorf("output_config = %+v (xhigh is high where xhigh does not exist)", got.OutputConfig)
	}
}

func TestAnthropicEffortBudgetMaxTokensInteraction(t *testing.T) {
	cases := []struct {
		name      string
		maxTokens string
		effort    string
		wantBudg  int // 0: thinking dropped
		wantMax   int
		warn      bool
	}{
		{"unset max_tokens", ``, "medium", 8192, 8192 + 4096, false},
		{"room to spare", `"max_tokens":20000,`, "high", 16384, 20000, false},
		{"budget above max_tokens comes down", `"max_tokens":8000,`, "high", 8000 - 1024, 8000, false},
		{"budget equal to max_tokens comes down", `"max_tokens":2048,`, "low", 1024, 2048, false},
		{"no room left drops thinking", `"max_tokens":1500,`, "high", 0, 1500, true},
		{"max_completion_tokens counts too", `"max_completion_tokens":9000,`, "xhigh", 9000 - 1024, 9000, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			js := `{"messages":[{"role":"user","content":"hi"}],` + c.maxTokens + `"reasoning_effort":"` + c.effort + `"}`
			cfg := Config{Model: "claude-sonnet-4-5"}
			got, err := translateAnthropic(context.Background(), cfg, parseReq(t, js))
			if err != nil {
				t.Fatal(err)
			}
			if c.wantBudg == 0 {
				if got.Thinking != nil {
					t.Errorf("thinking = %+v, want none", got.Thinking)
				}
			} else if got.Thinking == nil || got.Thinking.BudgetTokens != c.wantBudg {
				t.Errorf("thinking = %+v, want budget %d", got.Thinking, c.wantBudg)
			}
			if got.MaxTokens != c.wantMax {
				t.Errorf("max_tokens = %d, want %d", got.MaxTokens, c.wantMax)
			}
			w := warningsOf(t, Config{ProviderType: "anthropic", Model: "claude-sonnet-4-5"}, js)
			if (len(w) > 0) != c.warn {
				t.Errorf("warnings = %v, want warning: %v", w, c.warn)
			}
		})
	}
}

// TestEffortPlanTable pins the mapping of design section 5 for Anthropic.
func TestEffortPlanTable(t *testing.T) {
	type want struct {
		thinking string // type, or "" for none
		budget   int
		effort   string
		warn     bool
	}
	cases := []struct {
		model  string
		effort string
		want   want
	}{
		// adaptive, effort up to max, thinking can be disabled
		{"claude-opus-4-7", "none", want{"disabled", 0, "", false}},
		{"claude-opus-4-7", "minimal", want{"adaptive", 0, "low", false}},
		{"claude-opus-4-7", "low", want{"adaptive", 0, "low", false}},
		{"claude-opus-4-7", "medium", want{"adaptive", 0, "medium", false}},
		{"claude-opus-4-7", "high", want{"adaptive", 0, "high", false}},
		{"claude-opus-4-7", "xhigh", want{"adaptive", 0, "xhigh", false}},
		{"claude-opus-4-7", "max", want{"adaptive", 0, "max", false}},
		// 4.6 has no xhigh: it comes down to high with a Warning
		{"claude-opus-4-6", "xhigh", want{"adaptive", 0, "high", true}},
		{"claude-opus-4-6", "max", want{"adaptive", 0, "max", false}},
		// Opus 5.5 cannot switch thinking off
		{"claude-opus-5-5", "none", want{"adaptive", 0, "low", true}},
		{"claude-opus-5-5", "high", want{"adaptive", 0, "high", false}},
		// Sonnet 5.5 has between_tools in place of disabled
		{"claude-sonnet-5-5", "none", want{"between_tools", 0, "", false}},
		// a model the table does not know gets low, medium, high
		{"claude-future-9", "none", want{"adaptive", 0, "low", true}},
		{"claude-future-9", "max", want{"adaptive", 0, "high", true}},
		{"claude-future-9", "medium", want{"adaptive", 0, "medium", false}},
		// budget models
		{"claude-sonnet-4-5", "none", want{"", 0, "", false}},
		{"claude-sonnet-4-5", "minimal", want{"enabled", 1024, "", false}},
		{"claude-sonnet-4-5", "low", want{"enabled", 2048, "", false}},
		{"claude-sonnet-4-5", "medium", want{"enabled", 8192, "", false}},
		{"claude-sonnet-4-5", "high", want{"enabled", 16384, "", false}},
		{"claude-sonnet-4-5", "xhigh", want{"enabled", 32768, "", false}},
		{"claude-sonnet-4-5", "max", want{"enabled", 32768, "", false}},
		{"claude-opus-4-5", "medium", want{"enabled", 8192, "medium", false}},
		// no thinking at all: dropped with a Warning
		{"claude-3-5-sonnet-20241022", "high", want{"", 0, "", true}},
		{"claude-3-5-sonnet-20241022", "none", want{"", 0, "", false}},
	}
	for _, c := range cases {
		t.Run(c.model+"/"+c.effort, func(t *testing.T) {
			caps := CapabilitiesFor("anthropic", c.model)
			plan, err := effortPlan(caps, c.model, c.effort, true, 0)
			if err != nil {
				t.Fatal(err)
			}
			gotType, gotBudget := "", 0
			if plan.Thinking != nil {
				gotType, gotBudget = plan.Thinking.Type, plan.Thinking.BudgetTokens
			}
			if gotType != c.want.thinking || gotBudget != c.want.budget || plan.Effort != c.want.effort || (len(plan.Warnings) > 0) != c.want.warn {
				t.Errorf("plan = {%s %d %q warn=%v}, want %+v", gotType, gotBudget, plan.Effort, plan.Warnings, c.want)
			}
		})
	}
}

func TestEffortUnsetSendsNothing(t *testing.T) {
	for _, model := range []string{"claude-opus-4-7", "claude-sonnet-4-5", "claude-3-5-haiku"} {
		got, err := translateAnthropic(context.Background(), Config{Model: model},
			parseReq(t, `{"messages":[{"role":"user","content":"hi"}],"temperature":0.3}`))
		if err != nil {
			t.Fatal(err)
		}
		if got.Thinking != nil || got.OutputConfig != nil || got.MaxTokens != defaultMaxTokens {
			t.Errorf("%s: thinking=%+v output_config=%+v max_tokens=%d", model, got.Thinking, got.OutputConfig, got.MaxTokens)
		}
		if got.Temperature == nil {
			t.Errorf("%s: temperature dropped though no thinking was asked for", model)
		}
	}
}

func TestEffortTemperatureDroppedWithThinking(t *testing.T) {
	js := `{"messages":[{"role":"user","content":"hi"}],"temperature":0.4,"reasoning_effort":"low"}`
	got, err := translateAnthropic(context.Background(), Config{Model: "claude-opus-4-7"}, parseReq(t, js))
	if err != nil {
		t.Fatal(err)
	}
	if got.Temperature != nil {
		t.Errorf("temperature = %v, want dropped", *got.Temperature)
	}
	w := warningsOf(t, Config{ProviderType: "anthropic", Model: "claude-opus-4-7"}, js)
	if !hasWarning(w, "temperature") {
		t.Errorf("warnings = %v", w)
	}
	// With thinking switched off the temperature is fine.
	off := `{"messages":[{"role":"user","content":"hi"}],"temperature":0.4,"reasoning_effort":"none"}`
	got, _ = translateAnthropic(context.Background(), Config{Model: "claude-opus-4-7"}, parseReq(t, off))
	if got.Temperature == nil {
		t.Error("temperature dropped though thinking is off")
	}
}

func TestEffortTypoIs400(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
	defer srv.Close()
	for _, typ := range []string{"anthropic", "gemini", "ollama"} {
		for _, value := range []string{`"hgih"`, `5`} {
			p, _ := New(Config{ProviderType: typ, BaseURL: srv.URL, APIKey: "k", Model: "claude-opus-4-7"})
			req := parseReq(t, `{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":`+value+`}`)
			_, err := p.Chat(context.Background(), req)
			pe := asProviderError(t, err)
			if pe.Status != http.StatusBadRequest || pe.Type != "invalid_request_error" || !strings.Contains(pe.Message, "reasoning_effort") {
				t.Errorf("%s %s: error = %+v", typ, value, pe)
			}
			if w := warningsOf(t, Config{ProviderType: typ, BaseURL: srv.URL, Model: "m"}, `{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":`+value+`}`); len(w) != 0 {
				t.Errorf("%s: warnings for a rejected request: %v", typ, w)
			}
		}
	}
	if hit {
		t.Error("a rejected request reached the upstream")
	}
}

func TestEffortEmptyAndNullMeanUnset(t *testing.T) {
	for _, v := range []string{`""`, `null`} {
		r := parseReq(t, `{"messages":[],"reasoning_effort":`+v+`}`)
		if e, ok := r.ReasoningEffort(); ok {
			t.Errorf("%s read as %q", v, e)
		}
	}
}

func TestEffortThinkingNoneModelDropsWithWarning(t *testing.T) {
	js := `{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`
	got, err := translateAnthropic(context.Background(), Config{Model: "claude-3-5-sonnet-20241022"}, parseReq(t, js))
	if err != nil {
		t.Fatal(err)
	}
	if got.Thinking != nil || got.OutputConfig != nil {
		t.Errorf("thinking=%+v output_config=%+v", got.Thinking, got.OutputConfig)
	}
	w := warningsOf(t, Config{ProviderType: "anthropic", Model: "claude-3-5-sonnet-20241022"}, js)
	if !hasWarning(w, "reasoning_effort") {
		t.Errorf("warnings = %v", w)
	}
	// Gemini 2.0 has no thinking either.
	w = warningsOf(t, Config{ProviderType: "gemini", Model: "gemini-2.0-flash"}, js)
	if !hasWarning(w, "reasoning_effort") {
		t.Errorf("gemini warnings = %v", w)
	}
	g, err := translateGemini(context.Background(), Config{Model: "gemini-2.0-flash"}, parseReq(t, js))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := g.GenerationConfig["thinkingConfig"]; ok {
		t.Errorf("thinkingConfig sent to a model without thinking: %v", g.GenerationConfig)
	}
}

// ------------------------------------------------------------------ Gemini

func TestGeminiEffortMapping(t *testing.T) {
	cases := []struct {
		model, effort string
		want          string // JSON of thinkingConfig; "" for none
		warn          bool
	}{
		{"gemini-3-pro-preview", "high", `{"includeThoughts":true,"thinkingLevel":"high"}`, false},
		{"gemini-3-pro-preview", "medium", `{"includeThoughts":true,"thinkingLevel":"high"}`, false}, // pro has no medium
		{"gemini-3-pro-preview", "minimal", `{"includeThoughts":true,"thinkingLevel":"low"}`, false},
		{"gemini-3-pro-preview", "xhigh", `{"includeThoughts":true,"thinkingLevel":"high"}`, true},
		{"gemini-3-pro-preview", "none", `{"thinkingLevel":"low"}`, true},
		{"gemini-3-flash-preview", "medium", `{"includeThoughts":true,"thinkingLevel":"medium"}`, false},
		{"gemini-3-flash-preview", "minimal", `{"includeThoughts":true,"thinkingLevel":"minimal"}`, false},
		{"gemini-3-flash-preview", "none", `{"thinkingLevel":"minimal"}`, false},
		{"gemini-3-flash-preview", "max", `{"includeThoughts":true,"thinkingLevel":"high"}`, true},
		// a model nothing lists gets the set every Gemini 3 takes
		{"gemini-3.9-future", "medium", `{"includeThoughts":true,"thinkingLevel":"high"}`, false},
		// 2.5: a budget, clipped to the model
		{"gemini-2.5-flash", "high", `{"includeThoughts":true,"thinkingBudget":24576}`, false},
		{"gemini-2.5-flash", "xhigh", `{"includeThoughts":true,"thinkingBudget":24576}`, false},
		{"gemini-2.5-flash", "minimal", `{"includeThoughts":true,"thinkingBudget":512}`, false},
		{"gemini-2.5-flash", "low", `{"includeThoughts":true,"thinkingBudget":1024}`, false},
		{"gemini-2.5-flash", "medium", `{"includeThoughts":true,"thinkingBudget":8192}`, false},
		{"gemini-2.5-flash", "none", `{"thinkingBudget":0}`, false},
		{"gemini-2.5-pro", "max", `{"includeThoughts":true,"thinkingBudget":32768}`, false},
		{"gemini-2.5-pro", "none", `{"thinkingBudget":128}`, true},
		{"gemini-2.5-flash-lite", "none", `{"thinkingBudget":0}`, false},
	}
	for _, c := range cases {
		t.Run(c.model+"/"+c.effort, func(t *testing.T) {
			js := `{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"` + c.effort + `"}`
			g, err := translateGemini(context.Background(), Config{Model: c.model}, parseReq(t, js))
			if err != nil {
				t.Fatal(err)
			}
			b, _ := json.Marshal(g.GenerationConfig["thinkingConfig"])
			if string(b) != c.want {
				t.Errorf("thinkingConfig = %s, want %s", b, c.want)
			}
			if strings.Contains(string(b), "thinkingBudget") && strings.Contains(string(b), "thinkingLevel") {
				t.Errorf("both budget and level sent: %s", b)
			}
			w := warningsOf(t, Config{ProviderType: "gemini", Model: c.model}, js)
			if (len(w) > 0) != c.warn {
				t.Errorf("warnings = %v, want warning: %v", w, c.warn)
			}
		})
	}
}

func TestGeminiNoEffortNoThinkingConfig(t *testing.T) {
	g, err := translateGemini(context.Background(), Config{Model: "gemini-3-pro-preview"},
		parseReq(t, `{"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := g.GenerationConfig["thinkingConfig"]; ok {
		t.Errorf("thinkingConfig without reasoning_effort: %v", g.GenerationConfig)
	}
}

func TestGeminiThoughtsBecomeRagmuxThinking(t *testing.T) {
	fixture := readFixture(t, "gemini_thoughts.json")
	srv := geminiServer(t, func(w http.ResponseWriter, req geminiRequest) {
		_, _ = w.Write(fixture)
	})
	p, _ := New(Config{ProviderType: "gemini", BaseURL: srv.URL, APIKey: "k", Model: "gemini-3-pro-preview"})
	resp, err := p.Chat(context.Background(), parseReq(t, `{"model":"x","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`))
	if err != nil {
		t.Fatal(err)
	}
	m := resp.Choices[0].Message
	if m.Content == nil || *m.Content != "The answer is 42." {
		t.Errorf("content = %v (a thought leaked into the answer)", m.Content)
	}
	if len(m.Thinking) != 1 || m.Thinking[0].Type != "thinking" || m.Thinking[0].Thinking != "Weighing the question first." || m.Thinking[0].Signature != "" {
		t.Errorf("ragmux_thinking = %+v", m.Thinking)
	}
	b, _ := json.Marshal(m)
	if !strings.Contains(string(b), `"ragmux_thinking":[{"type":"thinking","thinking":"Weighing the question first."}]`) {
		t.Errorf("wire = %s", b)
	}
}

func TestGeminiStreamThoughts(t *testing.T) {
	fixture := readFixture(t, "gemini_thoughts.sse")
	srv := geminiServer(t, func(w http.ResponseWriter, req geminiRequest) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(fixture)
	})
	p, _ := New(Config{ProviderType: "gemini", BaseURL: srv.URL, APIKey: "k", Model: "gemini-2.5-flash"})
	out := make(chan StreamChunk, 16)
	err := p.ChatStream(context.Background(), parseReq(t, `{"model":"x","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"low"}`), out)
	close(out)
	if err != nil {
		t.Fatal(err)
	}
	blocks := mergeThinkingDeltas(t, out, new(string))
	if len(blocks) != 1 || blocks[0].Thinking != "Weighing the question." {
		t.Errorf("merged thinking = %+v", blocks)
	}
}

// mergeThinkingDeltas folds the ragmux_thinking deltas of a stream the way a
// client does (by index) and returns the blocks in index order. The answer
// text goes to *text.
func mergeThinkingDeltas(t *testing.T, chunks <-chan StreamChunk, text *string) []ThinkingBlock {
	t.Helper()
	byIndex := map[int]*ThinkingBlock{}
	var order []int
	for c := range chunks {
		for _, ch := range c.Choices {
			if ch.Delta.Content != nil {
				*text += *ch.Delta.Content
			}
			for _, d := range ch.Delta.Thinking {
				if d.Index == nil {
					t.Errorf("streamed thinking delta without index: %+v", d)
					continue
				}
				b, ok := byIndex[*d.Index]
				if !ok {
					b = &ThinkingBlock{}
					byIndex[*d.Index] = b
					order = append(order, *d.Index)
				}
				if d.Type != "" {
					b.Type = d.Type
				}
				b.Thinking += d.Thinking
				if d.Signature != "" {
					b.Signature = d.Signature
				}
				if d.Data != "" {
					b.Data = d.Data
				}
			}
		}
	}
	var out []ThinkingBlock
	for _, i := range order {
		out = append(out, *byIndex[i])
	}
	return out
}

// ------------------------------------------------------------------ Ollama

func TestOllamaThinkMapping(t *testing.T) {
	cases := []struct {
		model, effort string
		want          string // wire value of think; "" when absent
		warn          bool
	}{
		{"llama3.2", "high", `true`, false},
		{"llama3.2", "minimal", `true`, false},
		{"llama3.2", "none", `false`, false},
		{"gpt-oss:20b", "high", `"high"`, false},
		{"gpt-oss:20b", "minimal", `"low"`, false},
		{"gpt-oss:20b", "medium", `"medium"`, false},
		{"gpt-oss:20b", "max", `"high"`, true},
		{"gpt-oss:20b", "none", `false`, false},
	}
	for _, c := range cases {
		t.Run(c.model+"/"+c.effort, func(t *testing.T) {
			js := `{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"` + c.effort + `"}`
			o, err := translateOllama(context.Background(), Config{Model: c.model}, parseReq(t, js))
			if err != nil {
				t.Fatal(err)
			}
			if string(o.Think) != c.want {
				t.Errorf("think = %s, want %s", o.Think, c.want)
			}
			w := warningsOf(t, Config{ProviderType: "ollama", Model: c.model}, js)
			if (len(w) > 0) != c.warn {
				t.Errorf("warnings = %v, want warning: %v", w, c.warn)
			}
		})
	}
	o, _ := translateOllama(context.Background(), Config{Model: "llama3.2"}, parseReq(t, `{"messages":[{"role":"user","content":"hi"}]}`))
	raw, _ := json.Marshal(o)
	if strings.Contains(string(raw), `"think"`) {
		t.Errorf("think sent without reasoning_effort: %s", raw)
	}
}

func TestOllamaThinkingResponse(t *testing.T) {
	srv, _ := ollamaServer(t, func(w http.ResponseWriter, req ollamaRequest, raw map[string]json.RawMessage) {
		if string(raw["think"]) != "true" {
			t.Errorf("think = %s", raw["think"])
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"model": "m", "message": map[string]any{
			"role": "assistant", "content": "42", "thinking": "Let me count."}, "done": true})
	})
	p, _ := New(Config{ProviderType: "ollama", BaseURL: srv.URL, Model: "m"})
	resp, err := p.Chat(context.Background(), parseReq(t, `{"model":"x","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"low"}`))
	if err != nil {
		t.Fatal(err)
	}
	m := resp.Choices[0].Message
	if *m.Content != "42" || len(m.Thinking) != 1 || m.Thinking[0].Thinking != "Let me count." || m.Thinking[0].Signature != "" {
		t.Errorf("message = %+v", m)
	}
}

func TestOllamaStreamThinking(t *testing.T) {
	srv, _ := ollamaServer(t, func(w http.ResponseWriter, req ollamaRequest, _ map[string]json.RawMessage) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		for _, line := range []string{
			`{"model":"m","message":{"role":"assistant","content":"","thinking":"Let me "},"done":false}`,
			`{"model":"m","message":{"role":"assistant","content":"","thinking":"count."},"done":false}`,
			`{"model":"m","message":{"role":"assistant","content":"42"},"done":false}`,
			`{"model":"m","message":{"role":"assistant","content":""},"done":true,"done_reason":"stop","prompt_eval_count":3,"eval_count":4}`,
		} {
			_, _ = fmt.Fprintln(w, line)
		}
	})
	p, _ := New(Config{ProviderType: "ollama", BaseURL: srv.URL, Model: "m"})
	out := make(chan StreamChunk, 16)
	err := p.ChatStream(context.Background(), parseReq(t, `{"model":"x","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"low"}`), out)
	close(out)
	if err != nil {
		t.Fatal(err)
	}
	var text string
	blocks := mergeThinkingDeltas(t, out, &text)
	if text != "42" || len(blocks) != 1 || blocks[0].Thinking != "Let me count." {
		t.Errorf("text=%q blocks=%+v", text, blocks)
	}
}

func TestOllamaReplaysOnlyUnsignedThinking(t *testing.T) {
	js := fmt.Sprintf(`{"messages":[{"role":"user","content":"hi"},
		{"role":"assistant","content":"a","ragmux_thinking":[{"type":"thinking","thinking":"own reasoning"}]},
		{"role":"user","content":"again"},
		{"role":"assistant","content":"b","ragmux_thinking":[{"type":"thinking","thinking":"claude reasoning","signature":%q},{"type":"redacted_thinking","data":"x"}]},
		{"role":"user","content":"more"}]}`, fixtureThinkingSignature)
	o, err := translateOllama(context.Background(), Config{Model: "m"}, parseReq(t, js))
	if err != nil {
		t.Fatal(err)
	}
	if o.Messages[1].Thinking != "own reasoning" {
		t.Errorf("unsigned thinking = %q", o.Messages[1].Thinking)
	}
	if o.Messages[3].Thinking != "" {
		t.Errorf("a signed block reached Ollama: %q", o.Messages[3].Thinking)
	}
}

// ---------------------------------------------------- reasoning_content

func TestCompatReasoningContentForwardedAndFolded(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m map[string]json.RawMessage
		_ = json.Unmarshal(body, &m)
		mu.Lock()
		bodies = append(bodies, m)
		mu.Unlock()
		if strings.Contains(string(body), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, ev := range []string{
				`{"id":"c","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"Thin"}}]}`,
				`{"id":"c","choices":[{"index":0,"delta":{"reasoning_content":"king."}}]}`,
				`{"id":"c","choices":[{"index":0,"delta":{"content":"Done"},"finish_reason":"stop"}]}`,
			} {
				_, _ = w.Write([]byte("data: " + ev + "\n\n"))
			}
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			return
		}
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"Done","reasoning_content":"Thinking."},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	for _, typ := range []string{"custom_openai", "deepseek", AzureProviderType} {
		t.Run(typ, func(t *testing.T) {
			mu.Lock()
			bodies = nil
			mu.Unlock()
			base := srv.URL
			if typ == AzureProviderType {
				base = srv.URL + "?api-version=2024-10-21"
			}
			p, err := New(Config{ProviderType: typ, BaseURL: base, APIKey: "k", Model: "m"})
			if err != nil {
				t.Fatal(err)
			}
			// The history carries reasoning_content (kept) and ragmux_thinking (stripped);
			// a typo'd reasoning_effort is the upstream's to judge.
			js := fmt.Sprintf(`{"model":"x","reasoning_effort":"hgih","messages":[{"role":"user","content":"hi"},
				{"role":"assistant","content":"a","reasoning_content":"earlier","ragmux_thinking":[{"type":"thinking","thinking":"t","signature":%q}]},
				{"role":"user","content":"go on"}]}`, fixtureThinkingSignature)
			resp, err := p.Chat(context.Background(), parseReq(t, js))
			if err != nil {
				t.Fatal(err)
			}
			m := resp.Choices[0].Message
			if m.ReasoningContent == nil || *m.ReasoningContent != "Thinking." {
				t.Errorf("reasoning_content = %v", m.ReasoningContent)
			}
			if len(m.Thinking) != 1 || m.Thinking[0].Thinking != "Thinking." || m.Thinking[0].Signature != "" {
				t.Errorf("ragmux_thinking = %+v", m.Thinking)
			}
			mu.Lock()
			sent := bodies[0]
			mu.Unlock()
			if string(sent["reasoning_effort"]) != `"hgih"` {
				t.Errorf("reasoning_effort = %s, want it relayed as sent", sent["reasoning_effort"])
			}
			if !strings.Contains(string(sent["messages"]), `"reasoning_content":"earlier"`) {
				t.Errorf("reasoning_content not forwarded: %s", sent["messages"])
			}
			if strings.Contains(string(sent["messages"]), "ragmux_thinking") || strings.Contains(string(sent["messages"]), fixtureThinkingSignature) {
				t.Errorf("ragmux_thinking reached an OpenAI-compatible upstream: %s", sent["messages"])
			}

			out := make(chan StreamChunk, 16)
			if err := p.ChatStream(context.Background(), parseReq(t, js), out); err != nil {
				t.Fatal(err)
			}
			close(out)
			var reasoning, text string
			var folded []StreamChunk
			for c := range out {
				folded = append(folded, c)
				for _, ch := range c.Choices {
					if ch.Delta.ReasoningContent != nil {
						reasoning += *ch.Delta.ReasoningContent
					}
				}
			}
			ch := make(chan StreamChunk, len(folded))
			for _, c := range folded {
				ch <- c
			}
			close(ch)
			blocks := mergeThinkingDeltas(t, ch, &text)
			if reasoning != "Thinking." || text != "Done" || len(blocks) != 1 || blocks[0].Thinking != "Thinking." {
				t.Errorf("reasoning=%q text=%q blocks=%+v", reasoning, text, blocks)
			}
		})
	}
}

func TestReasoningContentDroppedWithWarningByTranslatingAdapters(t *testing.T) {
	js := `{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"a","reasoning_content":"earlier"},{"role":"user","content":"x"}]}`
	for _, typ := range []string{"anthropic", "gemini", "ollama"} {
		w := warningsOf(t, Config{ProviderType: typ, Model: "claude-opus-4-7"}, js)
		if !hasWarning(w, "reasoning_content") {
			t.Errorf("%s: warnings = %v", typ, w)
		}
	}
	a, err := translateAnthropic(context.Background(), Config{Model: "claude-opus-4-7"}, parseReq(t, js))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(a)
	if strings.Contains(string(raw), "earlier") && strings.Contains(string(raw), "reasoning") {
		t.Errorf("reasoning_content reached Anthropic: %s", raw)
	}
	// Empty reasoning_content is nothing to warn about.
	w := warningsOf(t, Config{ProviderType: "anthropic", Model: "claude-opus-4-7"},
		`{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"a","reasoning_content":""},{"role":"user","content":"x"}]}`)
	if len(w) != 0 {
		t.Errorf("warnings = %v", w)
	}
}

// ---------------------------------------------------- ragmux_thinking in

func TestRagmuxThinkingValidation(t *testing.T) {
	cases := []struct{ name, msg string }{
		{"not an array", `{"role":"assistant","content":"a","ragmux_thinking":"nope"}`},
		{"object instead of array", `{"role":"assistant","content":"a","ragmux_thinking":{"type":"thinking"}}`},
		{"element not an object", `{"role":"assistant","content":"a","ragmux_thinking":["x"]}`},
		{"unknown type", `{"role":"assistant","content":"a","ragmux_thinking":[{"type":"text","text":"x"}]}`},
		{"missing type", `{"role":"assistant","content":"a","ragmux_thinking":[{"thinking":"x"}]}`},
		{"thinking not a string", `{"role":"assistant","content":"a","ragmux_thinking":[{"type":"thinking","thinking":5}]}`},
		{"signature not a string", `{"role":"assistant","content":"a","ragmux_thinking":[{"type":"thinking","thinking":"x","signature":[]}]}`},
		{"redacted without data", `{"role":"assistant","content":"a","ragmux_thinking":[{"type":"redacted_thinking"}]}`},
		{"redacted with empty data", `{"role":"assistant","content":"a","ragmux_thinking":[{"type":"redacted_thinking","data":""}]}`},
		{"on a user message", `{"role":"user","content":"a","ragmux_thinking":[{"type":"thinking","thinking":"x","signature":"s"}]}`},
		{"on a tool message", `{"role":"tool","tool_call_id":"c","content":"a","ragmux_thinking":[]}`},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("an invalid request reached the upstream")
	}))
	defer srv.Close()
	for _, c := range cases {
		for _, typ := range []string{"anthropic", "gemini", "ollama", "custom_openai"} {
			t.Run(c.name+"/"+typ, func(t *testing.T) {
				p, _ := New(Config{ProviderType: typ, BaseURL: srv.URL, APIKey: "k", Model: "m"})
				req := parseReq(t, `{"messages":[{"role":"user","content":"hi"},`+c.msg+`,{"role":"user","content":"x"}]}`)
				_, err := p.Chat(context.Background(), req)
				pe := asProviderError(t, err)
				if pe.Status != http.StatusBadRequest || pe.Code != "invalid_ragmux_thinking" {
					t.Errorf("error = %+v", pe)
				}
			})
		}
	}
	// null and an empty array are nothing, not an error.
	for _, ok := range []string{`null`, `[]`} {
		req := parseReq(t, `{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"a","ragmux_thinking":`+ok+`},{"role":"user","content":"x"}]}`)
		if err := ValidateRequestThinking(req.Messages); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
}

// anthropicRecorder answers every request with a fixture (the SSE one when
// the request streams) and keeps the raw bodies.
type anthropicRecorder struct {
	mu     sync.Mutex
	bodies [][]byte
}

func (r *anthropicRecorder) server(t *testing.T) *httptest.Server {
	t.Helper()
	jsonFix := readFixture(t, "anthropic_thinking.json")
	sseFix := readFixture(t, "anthropic_thinking.sse")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.bodies = append(r.bodies, body)
		r.mu.Unlock()
		if strings.Contains(string(body), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write(sseFix)
			return
		}
		_, _ = w.Write(jsonFix)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (r *anthropicRecorder) last() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.bodies[len(r.bodies)-1]
}

type wireBlock struct {
	Type      string  `json:"type"`
	Text      string  `json:"text"`
	Thinking  *string `json:"thinking"`
	Signature string  `json:"signature"`
	Data      string  `json:"data"`
}

func assistantBlocks(t *testing.T, body []byte) []wireBlock {
	t.Helper()
	var req struct {
		Messages []struct {
			Role    string      `json:"role"`
			Content []wireBlock `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	for _, m := range req.Messages {
		if m.Role == "assistant" {
			return m.Content
		}
	}
	return nil
}

// secondTurnRequest is what a client sends after a tool round: the assistant
// message exactly as the gateway gave it to the client (marshalled from the
// response, so the test goes through the real wire form), plus the tool
// result. When thinking is non-nil it replaces the message's ragmux_thinking.
func secondTurnRequest(t *testing.T, assistant ResponseMessage, thinking []ThinkingBlock) ChatRequest {
	t.Helper()
	assistant.Thinking = thinking
	am, err := json.Marshal(assistant)
	if err != nil {
		t.Fatal(err)
	}
	js := `{"model":"x","messages":[{"role":"user","content":"weather?"},` + string(am) +
		`,{"role":"tool","tool_call_id":"toolu_01","content":"sunny"}]}`
	return parseReq(t, js)
}

func assertReplayedThinking(t *testing.T, body []byte) {
	t.Helper()
	// Byte for byte: the raw bytes of the signature and the payload are in the body.
	if !strings.Contains(string(body), `"signature":"`+fixtureThinkingSignature+`"`) {
		t.Errorf("signature changed on the way back: %s", body)
	}
	if !strings.Contains(string(body), `"data":"`+fixtureRedactedData+`"`) {
		t.Errorf("redacted data changed on the way back: %s", body)
	}
	blocks := assistantBlocks(t, body)
	var types []string
	for _, b := range blocks {
		types = append(types, b.Type)
	}
	if strings.Join(types, ",") != "thinking,redacted_thinking,text,tool_use" {
		t.Errorf("assistant content order = %v", types)
	}
	if len(blocks) > 0 && (blocks[0].Thinking == nil || blocks[0].Signature != fixtureThinkingSignature) {
		t.Errorf("thinking block = %+v", blocks[0])
	}
}

func TestAnthropicThinkingRoundTripNonStream(t *testing.T) {
	rec := &anthropicRecorder{}
	srv := rec.server(t)
	p, _ := New(Config{ProviderType: "anthropic", BaseURL: srv.URL, APIKey: "k", Model: "claude-opus-4-7"})
	first := parseReq(t, `{"model":"x","messages":[{"role":"user","content":"weather?"}],"reasoning_effort":"low"}`)
	resp, err := p.Chat(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	m := resp.Choices[0].Message
	if len(m.Thinking) != 2 {
		t.Fatalf("ragmux_thinking = %+v", m.Thinking)
	}
	if b := m.Thinking[0]; b.Type != "thinking" || b.Thinking != "The user wants the weather, so call the tool." || b.Signature != fixtureThinkingSignature {
		t.Errorf("block 0 = %+v", b)
	}
	if b := m.Thinking[1]; b.Type != "redacted_thinking" || b.Data != fixtureRedactedData {
		t.Errorf("block 1 = %+v", b)
	}
	wire, _ := json.Marshal(m)
	if !strings.Contains(string(wire), `"ragmux_thinking":[{"type":"thinking"`) {
		t.Errorf("response wire = %s", wire)
	}

	if _, err := p.Chat(context.Background(), secondTurnRequest(t, m, m.Thinking)); err != nil {
		t.Fatal(err)
	}
	assertReplayedThinking(t, rec.last())
}

func TestAnthropicThinkingRoundTripStream(t *testing.T) {
	rec := &anthropicRecorder{}
	srv := rec.server(t)
	p, _ := New(Config{ProviderType: "anthropic", BaseURL: srv.URL, APIKey: "k", Model: "claude-opus-4-7"})
	out := make(chan StreamChunk, 32)
	err := p.ChatStream(context.Background(), parseReq(t, `{"model":"x","messages":[{"role":"user","content":"weather?"}],"reasoning_effort":"low"}`), out)
	close(out)
	if err != nil {
		t.Fatal(err)
	}
	var chunks []StreamChunk
	var calls json.RawMessage
	for c := range out {
		chunks = append(chunks, c)
		for _, ch := range c.Choices {
			if len(ch.Delta.ToolCalls) > 0 && calls == nil {
				calls = ch.Delta.ToolCalls
			}
		}
	}
	// The thinking deltas, as they go to the client.
	var thinkingWire []string
	for _, c := range chunks {
		for _, ch := range c.Choices {
			if len(ch.Delta.Thinking) > 0 {
				b, _ := json.Marshal(ch.Delta.Thinking)
				thinkingWire = append(thinkingWire, string(b))
			}
		}
	}
	wantPrefix := []string{
		`[{"index":0,"type":"thinking"}]`,
		`[{"index":0,"type":"thinking","thinking":"The user wants "}]`,
		`[{"index":0,"type":"thinking","thinking":"the weather."}]`,
		`[{"index":0,"type":"thinking","signature":"` + fixtureThinkingSignature + `"}]`,
		`[{"index":1,"type":"redacted_thinking","data":"` + fixtureRedactedData + `"}]`,
	}
	if strings.Join(thinkingWire, "\n") != strings.Join(wantPrefix, "\n") {
		t.Errorf("thinking deltas:\n%s\nwant:\n%s", strings.Join(thinkingWire, "\n"), strings.Join(wantPrefix, "\n"))
	}

	ch := make(chan StreamChunk, len(chunks))
	for _, c := range chunks {
		ch <- c
	}
	close(ch)
	var text string
	blocks := mergeThinkingDeltas(t, ch, &text)
	if len(blocks) != 2 || blocks[0].Signature != fixtureThinkingSignature || blocks[0].Thinking != "The user wants the weather." || blocks[1].Data != fixtureRedactedData {
		t.Fatalf("merged = %+v", blocks)
	}
	// What the client now holds goes back with the tool result. The tool call
	// is the one that streamed (index 0 on the OpenAI side).
	assistant := ResponseMessage{Role: "assistant", Content: strPtr("Let me check."), ToolCalls: toolCallsJSON([]toolCall{{ID: "toolu_01", Name: "get_weather", Arguments: `{"city":"Ankara"}`}})}
	if _, err := p.Chat(context.Background(), secondTurnRequest(t, assistant, blocks)); err != nil {
		t.Fatal(err)
	}
	assertReplayedThinking(t, rec.last())
}

func TestAnthropicDropsUnsignedThinkingOnReplay(t *testing.T) {
	js := fmt.Sprintf(`{"messages":[{"role":"user","content":"hi"},
		{"role":"assistant","content":"a","ragmux_thinking":[
			{"type":"thinking","thinking":"from gemini"},
			{"type":"thinking","thinking":"from claude","signature":%q},
			{"type":"redacted_thinking","data":%q}]},
		{"role":"user","content":"again"}]}`, fixtureThinkingSignature, fixtureRedactedData)
	a, err := translateAnthropic(context.Background(), Config{Model: "claude-opus-4-7"}, parseReq(t, js))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(a)
	if strings.Contains(string(raw), "from gemini") {
		t.Errorf("unsigned thinking sent to Anthropic: %s", raw)
	}
	blocks := assistantBlocks(t, raw)
	if len(blocks) != 3 || blocks[0].Type != "thinking" || blocks[1].Type != "redacted_thinking" || blocks[2].Type != "text" {
		t.Errorf("blocks = %+v", blocks)
	}
}

func TestAnthropicEmptyThinkingTextIsKept(t *testing.T) {
	// Claude 5 returns thinking text omitted: "thinking" is empty, the signature carries the block.
	js := fmt.Sprintf(`{"messages":[{"role":"user","content":"hi"},
		{"role":"assistant","content":"a","ragmux_thinking":[{"type":"thinking","signature":%q}]},
		{"role":"user","content":"again"}]}`, fixtureThinkingSignature)
	a, err := translateAnthropic(context.Background(), Config{Model: "claude-opus-4-7"}, parseReq(t, js))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(a)
	if !strings.Contains(string(raw), `{"type":"thinking","thinking":"","signature":"`) {
		t.Errorf("an empty-text thinking block lost its thinking field: %s", raw)
	}
}

func TestAnthropicThinkingOnlyAssistantMessageIsNotATurn(t *testing.T) {
	js := fmt.Sprintf(`{"messages":[{"role":"user","content":"hi"},
		{"role":"assistant","content":"","ragmux_thinking":[{"type":"thinking","thinking":"x","signature":%q}]},
		{"role":"user","content":"again"}]}`, fixtureThinkingSignature)
	a, err := translateAnthropic(context.Background(), Config{Model: "claude-opus-4-7"}, parseReq(t, js))
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Messages) != 1 || a.Messages[0].Role != "user" {
		t.Errorf("messages = %+v", a.Messages)
	}
}

// ------------------------------------------------- RAG x prefix binding

func ragThinkingRequest(t *testing.T, ragActive bool) ChatRequest {
	t.Helper()
	req := parseReq(t, fmt.Sprintf(`{"messages":[{"role":"user","content":"weather?"},
		{"role":"assistant","content":"Let me check.","tool_calls":[{"id":"toolu_01","type":"function","function":{"name":"get_weather","arguments":"{}"}}],
		 "ragmux_thinking":[{"type":"thinking","thinking":"t","signature":%q},{"type":"redacted_thinking","data":%q}]},
		{"role":"tool","tool_call_id":"toolu_01","content":"sunny"}]}`, fixtureThinkingSignature, fixtureRedactedData))
	req.RAGActive = ragActive
	return req
}

func TestRAGPrefixBoundModelDropsPriorThinking(t *testing.T) {
	cfg := Config{ProviderType: "anthropic", Model: "claude-opus-5-5"}
	req := ragThinkingRequest(t, true)
	a, err := translateAnthropic(context.Background(), cfg, req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(a)
	if strings.Contains(string(raw), "thinking") || strings.Contains(string(raw), fixtureThinkingSignature) {
		t.Errorf("thinking blocks sent to a prefix-bound model in a RAG project: %s", raw)
	}
	blocks := assistantBlocks(t, raw)
	if len(blocks) != 2 || blocks[0].Type != "text" || blocks[1].Type != "tool_use" {
		t.Errorf("the rest of the turn must stay: %+v", blocks)
	}
	p, _ := New(cfg)
	w := p.(RequestWarner).RequestWarnings(req)
	if !hasWarning(w, "thinking blocks of earlier turns were dropped") {
		t.Errorf("warnings = %v", w)
	}
}

func TestRAGPrefixBoundKeepsThinkingWhenRAGIsOff(t *testing.T) {
	cfg := Config{ProviderType: "anthropic", Model: "claude-opus-5-5"}
	req := ragThinkingRequest(t, false)
	a, err := translateAnthropic(context.Background(), cfg, req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(a)
	if !strings.Contains(string(raw), fixtureThinkingSignature) {
		t.Errorf("thinking dropped though retrieval is not active: %s", raw)
	}
	p, _ := New(cfg)
	if w := p.(RequestWarner).RequestWarnings(req); hasWarning(w, "dropped") {
		t.Errorf("warnings = %v", w)
	}
}

func TestRAGLegacyModelKeepsThinking(t *testing.T) {
	// Without prefix binding the tool turn needs its block, so nothing is removed.
	for _, model := range []string{"claude-sonnet-4-5", "claude-opus-4-7"} {
		cfg := Config{ProviderType: "anthropic", Model: model}
		req := ragThinkingRequest(t, true)
		a, err := translateAnthropic(context.Background(), cfg, req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(a)
		if !strings.Contains(string(raw), fixtureThinkingSignature) || !strings.Contains(string(raw), fixtureRedactedData) {
			t.Errorf("%s: thinking dropped: %s", model, raw)
		}
		p, _ := New(cfg)
		if w := p.(RequestWarner).RequestWarnings(req); hasWarning(w, "dropped") {
			t.Errorf("%s: warnings = %v", model, w)
		}
	}
}

func TestRAGActiveStaysOffTheWire(t *testing.T) {
	req := parseReq(t, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	req.RAGActive = true
	b, _ := json.Marshal(req)
	if strings.Contains(strings.ToLower(string(b)), "ragactive") || strings.Contains(string(b), "rag_active") {
		t.Errorf("RAGActive serialised: %s", b)
	}
}

// -------------------------------------------------------- capabilities

func TestGeminiEffortLevels(t *testing.T) {
	cases := map[string][]string{
		"gemini-3-pro-preview":   {"low", "high"},
		"gemini-3-flash-preview": {"minimal", "low", "medium", "high"},
		"gemini-3.1-pro-preview": {"low", "medium", "high"},
		"gemini-3.9-unlisted":    {"low", "high"},
		"":                       {"low", "high"},
	}
	for model, want := range cases {
		got := CapabilitiesFor("gemini", model)
		if got.Thinking != ThinkingLevel || strings.Join(got.EffortLevels, ",") != strings.Join(want, ",") {
			t.Errorf("%q: thinking=%s levels=%v, want %v", model, got.Thinking, got.EffortLevels, want)
		}
	}
	if got := CapabilitiesFor("gemini", "gemini-2.5-flash"); got.Thinking != ThinkingBudget || len(got.EffortLevels) != 0 {
		t.Errorf("gemini 2.5 = %+v", got)
	}
}

// ------------------------------------------- manual-mode incompatibilities

const toolsJSON = `"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}]`

func TestBudgetThinkingGivesWayToForcedToolChoice(t *testing.T) {
	for _, choice := range []string{`"required"`, `{"type":"function","function":{"name":"get_weather"}}`} {
		js := `{"messages":[{"role":"user","content":"hi"}],` + toolsJSON + `,"tool_choice":` + choice + `,"temperature":0.3,"reasoning_effort":"high"}`
		a, err := translateAnthropic(context.Background(), Config{Model: "claude-sonnet-4-5"}, parseReq(t, js))
		if err != nil {
			t.Fatal(err)
		}
		if a.Thinking != nil || a.OutputConfig != nil {
			t.Errorf("%s: thinking=%+v output_config=%+v next to a forced tool_choice", choice, a.Thinking, a.OutputConfig)
		}
		if a.ToolChoice == nil {
			t.Errorf("%s: the forced tool_choice was dropped instead", choice)
		}
		if a.MaxTokens != defaultMaxTokens || a.Temperature == nil {
			t.Errorf("%s: max_tokens=%d temperature=%v, want the request as it would be without thinking", choice, a.MaxTokens, a.Temperature)
		}
		w := warningsOf(t, Config{ProviderType: "anthropic", Model: "claude-sonnet-4-5"}, js)
		if !hasWarning(w, "forced tool_choice") || hasWarning(w, "temperature") {
			t.Errorf("warnings = %v", w)
		}
	}
	// auto keeps thinking; so does an adaptive model, which accepts both.
	auto := `{"messages":[{"role":"user","content":"hi"}],` + toolsJSON + `,"tool_choice":"auto","reasoning_effort":"high"}`
	if a, _ := translateAnthropic(context.Background(), Config{Model: "claude-sonnet-4-5"}, parseReq(t, auto)); a.Thinking == nil {
		t.Error("thinking dropped with tool_choice auto")
	}
	forced := `{"messages":[{"role":"user","content":"hi"}],` + toolsJSON + `,"tool_choice":"required","reasoning_effort":"high"}`
	a, _ := translateAnthropic(context.Background(), Config{Model: "claude-opus-4-7"}, parseReq(t, forced))
	if a.Thinking == nil || a.ToolChoice == nil {
		t.Errorf("adaptive model: thinking=%+v tool_choice=%s", a.Thinking, a.ToolChoice)
	}
}

// Effort is a parameter of its own on Opus 4.5: dropping the thinking budget
// must not take the effort with it, and the only effort Warning left is the
// one about the lowering.
func TestDroppedBudgetThinkingKeepsEffort(t *testing.T) {
	js := `{"messages":[{"role":"user","content":"hi"}],` + toolsJSON + `,"tool_choice":"required","reasoning_effort":"low"}`
	a, err := translateAnthropic(context.Background(), Config{Model: "claude-opus-4-5"}, parseReq(t, js))
	if err != nil {
		t.Fatal(err)
	}
	if a.Thinking != nil || a.OutputConfig == nil || a.OutputConfig.Effort != "low" {
		t.Errorf("thinking=%+v output_config=%+v", a.Thinking, a.OutputConfig)
	}
	w := warningsOf(t, Config{ProviderType: "anthropic", Model: "claude-opus-4-5"}, js)
	if len(w) != 1 || !hasWarning(w, "thinking disabled") {
		t.Errorf("warnings = %v", w)
	}
	// With max the lowering is reported once, and nothing says the effort was ignored.
	maxJS := strings.Replace(js, `"low"`, `"max"`, 1)
	w = warningsOf(t, Config{ProviderType: "anthropic", Model: "claude-opus-4-5"}, maxJS)
	if len(w) != 2 || hasWarning(w, "reasoning_effort ignored") {
		t.Errorf("warnings = %v", w)
	}
}

func toolRoundJSON(thinking string) string {
	return `{"messages":[{"role":"user","content":"weather?"},
		{"role":"assistant","content":"Let me check.","tool_calls":[{"id":"toolu_01","type":"function","function":{"name":"get_weather","arguments":"{}"}}]` + thinking + `},
		{"role":"tool","tool_call_id":"toolu_01","content":"sunny"}],` + toolsJSON + `,"reasoning_effort":"medium"}`
}

func TestBudgetThinkingNeedsItsBlockOnATurnRound(t *testing.T) {
	cfg := Config{ProviderType: "anthropic", Model: "claude-sonnet-4-5"}
	bare := toolRoundJSON(``)
	a, err := translateAnthropic(context.Background(), cfg, parseReq(t, bare))
	if err != nil {
		t.Fatal(err)
	}
	if a.Thinking != nil || a.MaxTokens != defaultMaxTokens {
		t.Errorf("thinking=%+v max_tokens=%d for a tool round without a thinking block", a.Thinking, a.MaxTokens)
	}
	if w := warningsOf(t, cfg, bare); !hasWarning(w, "tool round") {
		t.Errorf("warnings = %v", w)
	}

	// An unsigned block is not one Anthropic can use.
	unsigned := toolRoundJSON(`,"ragmux_thinking":[{"type":"thinking","thinking":"from elsewhere"}]`)
	if a, _ := translateAnthropic(context.Background(), cfg, parseReq(t, unsigned)); a.Thinking != nil {
		t.Errorf("thinking sent with only an unsigned block: %+v", a.Thinking)
	}

	signed := toolRoundJSON(fmt.Sprintf(`,"ragmux_thinking":[{"type":"thinking","thinking":"t","signature":%q}]`, fixtureThinkingSignature))
	a, err = translateAnthropic(context.Background(), cfg, parseReq(t, signed))
	if err != nil {
		t.Fatal(err)
	}
	if a.Thinking == nil || a.Thinking.BudgetTokens != 8192 {
		t.Errorf("thinking = %+v, want it kept when the block is sent back", a.Thinking)
	}
	if w := warningsOf(t, cfg, signed); len(w) != 0 {
		t.Errorf("warnings = %v", w)
	}

	// Adaptive models do not need the block, and a request that is not
	// a tool round never did.
	if a, _ := translateAnthropic(context.Background(), Config{Model: "claude-opus-4-7"}, parseReq(t, bare)); a.Thinking == nil {
		t.Error("adaptive thinking dropped on a tool round")
	}
	plain := `{"messages":[{"role":"user","content":"a"},{"role":"assistant","content":"b"},{"role":"user","content":"c"}],"reasoning_effort":"medium"}`
	if a, _ := translateAnthropic(context.Background(), Config{Model: "claude-sonnet-4-5"}, parseReq(t, plain)); a.Thinking == nil {
		t.Error("thinking dropped on a turn that is not a tool round")
	}
}

func TestThinkingTopPFloor(t *testing.T) {
	cases := []struct {
		model, topP, effort string
		keep                bool
		warn                bool
	}{
		{"claude-sonnet-4-5", "0.9", "high", false, true},
		{"claude-sonnet-4-5", "0.95", "high", true, false},
		{"claude-sonnet-4-5", "1", "high", true, false},
		{"claude-opus-4-6", "0.5", "low", false, true},
		{"claude-sonnet-4-5", "0.9", "none", true, false}, // no thinking, no limit
		{"claude-opus-4-7", "0.9", "none", true, false},   // thinking disabled
	}
	for _, c := range cases {
		js := `{"messages":[{"role":"user","content":"hi"}],"top_p":` + c.topP + `,"reasoning_effort":"` + c.effort + `"}`
		a, err := translateAnthropic(context.Background(), Config{Model: c.model}, parseReq(t, js))
		if err != nil {
			t.Fatal(err)
		}
		if (a.TopP != nil) != c.keep {
			t.Errorf("%s top_p=%s effort=%s: top_p kept = %v", c.model, c.topP, c.effort, a.TopP != nil)
		}
		w := warningsOf(t, Config{ProviderType: "anthropic", Model: c.model}, js)
		if hasWarning(w, "top_p") != c.warn {
			t.Errorf("%s top_p=%s: warnings = %v", c.model, c.topP, w)
		}
	}
}

func TestGeminiModelsPrefixResolvesCapabilities(t *testing.T) {
	if got := CapabilitiesFor("gemini", "models/gemini-2.5-pro"); got.Thinking != ThinkingBudget {
		t.Errorf("models/gemini-2.5-pro thinking = %s", got.Thinking)
	}
	js := `{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`
	g, err := translateGemini(context.Background(), Config{Model: "models/gemini-2.0-flash"}, parseReq(t, js))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := g.GenerationConfig["thinkingConfig"]; ok {
		t.Errorf("thinkingConfig sent to a model without thinking: %v", g.GenerationConfig)
	}
	if w := warningsOf(t, Config{ProviderType: "gemini", Model: "models/gemini-2.0-flash"}, js); !hasWarning(w, "reasoning_effort") {
		t.Errorf("warnings = %v", w)
	}
	g, _ = translateGemini(context.Background(), Config{Model: "models/gemini-2.5-pro"}, parseReq(t, js))
	if b, _ := json.Marshal(g.GenerationConfig["thinkingConfig"]); string(b) != `{"includeThoughts":true,"thinkingBudget":24576}` {
		t.Errorf("thinkingConfig = %s", b)
	}
}

func TestOpus45EffortLoweringWarns(t *testing.T) {
	js := `{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"max"}`
	w := warningsOf(t, Config{ProviderType: "anthropic", Model: "claude-opus-4-5"}, js)
	if !hasWarning(w, "reasoning_effort max") {
		t.Errorf("warnings = %v", w)
	}
	high := `{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`
	if w := warningsOf(t, Config{ProviderType: "anthropic", Model: "claude-opus-4-5"}, high); len(w) != 0 {
		t.Errorf("warnings = %v", w)
	}
}
