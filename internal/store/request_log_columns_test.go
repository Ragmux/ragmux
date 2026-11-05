package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/internal/testdb"
)

// TestRequestedModelIsBounded checks that the client's model string cannot
// make a request log row arbitrarily large, and that a NUL byte in it does
// not cost the row.
func TestRequestedModelIsBounded(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	conn, err := s.CreateConnection(ctx, &store.ModelConnection{Name: "m", ProviderType: "openai", ModelName: "gpt-4o"})
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := s.CreateProject(ctx, &store.Project{Name: "p", ModelConnectionID: conn.ID})
	if err != nil {
		t.Fatal(err)
	}

	for id, model := range map[string]string{
		"long":  strings.Repeat("m", 1<<20),
		"multi": strings.Repeat("é", 1000),
		"nul":   "gpt\x00-4o",
		"plain": "gpt-4o",
		"empty": "",
	} {
		if err := s.InsertRequestLog(ctx, &store.RequestLog{ProjectID: p.ID, ModelName: "m", StatusCode: 200,
			RequestID: id, RequestedModel: model}); err != nil {
			t.Fatalf("%s: insert: %v", id, err)
		}
	}

	stored := func(id string) (string, bool) {
		var v *string
		if err := s.DB().QueryRow(ctx, "SELECT requested_model FROM request_logs WHERE request_id = $1", id).Scan(&v); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if v == nil {
			return "", false
		}
		return *v, true
	}
	if v, _ := stored("long"); len(v) != store.MaxRequestedModelBytes || strings.Trim(v, "m") != "" {
		t.Errorf("long model stored with %d bytes, want %d", len(v), store.MaxRequestedModelBytes)
	}
	if v, _ := stored("multi"); len(v) > store.MaxRequestedModelBytes || strings.Trim(v, "é") != "" {
		t.Errorf("multi-byte model stored with %d bytes or a split character", len(v))
	}
	if v, _ := stored("nul"); v != "gpt-4o" {
		t.Errorf("model with a NUL stored as %q, want %q", v, "gpt-4o")
	}
	if v, _ := stored("plain"); v != "gpt-4o" {
		t.Errorf("plain model stored as %q", v)
	}
	if _, present := stored("empty"); present {
		t.Error("an empty model should be stored as NULL")
	}
}

// TestGuardrailColumns checks migration 0032 end to end: a row that says
// nothing about guardrails reads as "none triggered", the counters and the
// rule ids round-trip, and the rule list is bounded and de-duplicated.
func TestGuardrailColumns(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	conn, err := s.CreateConnection(ctx, &store.ModelConnection{Name: "m", ProviderType: "openai", ModelName: "gpt-4o"})
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := s.CreateProject(ctx, &store.Project{Name: "p", ModelConnectionID: conn.ID})
	if err != nil {
		t.Fatal(err)
	}
	many := make([]string, 0, 40)
	for i := 0; i < 40; i++ {
		many = append(many, "rule-"+strings.Repeat("x", i%3)+string(rune('a'+i%26)))
	}
	many = append(many, "dup", "dup", "nul\x00byte")
	for _, l := range []*store.RequestLog{
		{ProjectID: p.ID, ModelName: "m", StatusCode: 200, RequestID: "plain"},
		{ProjectID: p.ID, ModelName: "m", StatusCode: 400, RequestID: "blocked", GuardrailBlockedStage: "pre_request",
			GuardrailRules: []string{"tckn", "tckn"}},
		{ProjectID: p.ID, ModelName: "m", StatusCode: 200, RequestID: "counted", GuardrailRedactions: 3, GuardrailFlags: 2,
			GuardrailRules: []string{"iban", "words", "webhook:pii"}, GuardrailWebhook: "ok"},
		{ProjectID: p.ID, ModelName: "m", StatusCode: 200, RequestID: "many", GuardrailRules: many, GuardrailRedactions: -4},
	} {
		if err := s.InsertRequestLog(ctx, l); err != nil {
			t.Fatalf("%s: %v", l.RequestID, err)
		}
	}
	type row struct {
		stage    *string
		redacted int
		flags    int
		rules    []string
		webhook  *string
	}
	read := func(id string) row {
		var r row
		if err := s.DB().QueryRow(ctx, `SELECT guardrail_blocked_stage, guardrail_redactions, guardrail_flags,
			guardrail_rules, guardrail_webhook FROM request_logs WHERE request_id = $1`, id).
			Scan(&r.stage, &r.redacted, &r.flags, &r.rules, &r.webhook); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		return r
	}
	if r := read("plain"); r.stage != nil || r.redacted != 0 || r.flags != 0 || r.rules != nil || r.webhook != nil {
		t.Errorf("a row without guardrails: %+v", r)
	}
	if r := read("blocked"); r.stage == nil || *r.stage != "pre_request" || strings.Join(r.rules, ",") != "tckn" {
		t.Errorf("blocked row: %+v", r)
	}
	if r := read("counted"); r.redacted != 3 || r.flags != 2 || strings.Join(r.rules, ",") != "iban,words,webhook:pii" ||
		r.webhook == nil || *r.webhook != "ok" {
		t.Errorf("counted row: %+v", r)
	}
	r := read("many")
	if len(r.rules) != store.MaxGuardrailRules {
		t.Errorf("rule list has %d entries, want the cap %d", len(r.rules), store.MaxGuardrailRules)
	}
	if r.redacted != 0 {
		t.Errorf("a negative count must be stored as 0, got %d", r.redacted)
	}
	seen := map[string]bool{}
	for _, id := range r.rules {
		if seen[id] || strings.ContainsRune(id, 0) {
			t.Errorf("rule list has a duplicate or a NUL byte: %q", r.rules)
		}
		seen[id] = true
	}
}

// TestProjectGuardrails: the configuration is stored as given, survives a
// project update that does not mention it, and can be cleared.
func TestProjectGuardrails(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	conn, err := s.CreateConnection(ctx, &store.ModelConnection{Name: "m", ProviderType: "openai", ModelName: "gpt-4o"})
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := s.CreateProject(ctx, &store.Project{Name: "p", ModelConnectionID: conn.ID})
	if err != nil {
		t.Fatal(err)
	}
	if p.Guardrails != nil {
		t.Fatalf("a new project has guardrails: %s", p.Guardrails)
	}
	cfg := json.RawMessage(`{"rules":[{"id":"t","type":"tckn","action":"redact","stages":["pre_request"]}],"stream_window_chars":64}`)
	if err := s.SetProjectGuardrails(ctx, p.ID, cfg); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetProject(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	var a, b map[string]any
	if json.Unmarshal(got.Guardrails, &a) != nil || json.Unmarshal(cfg, &b) != nil || len(a) != 2 || a["stream_window_chars"] != b["stream_window_chars"] {
		t.Errorf("stored %s, want %s", got.Guardrails, cfg)
	}
	// An ordinary project update leaves the column alone.
	got.Name = "renamed"
	got.Guardrails = nil
	if _, err := s.UpdateProject(ctx, got); err != nil {
		t.Fatal(err)
	}
	if again, _ := s.GetProject(ctx, p.ID); again.Name != "renamed" || again.Guardrails == nil {
		t.Errorf("project update lost the guardrails: %+v", again)
	}
	if err := s.SetProjectGuardrails(ctx, p.ID, nil); err != nil {
		t.Fatal(err)
	}
	if cleared, _ := s.GetProject(ctx, p.ID); cleared.Guardrails != nil {
		t.Errorf("cleared guardrails read back as %s", cleared.Guardrails)
	}
	if err := s.SetProjectGuardrails(ctx, p.ID+999, cfg); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown project: %v", err)
	}
}
