package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/ragmux/ragmux/internal/guardrail"
	"github.com/ragmux/ragmux/internal/store"
)

func TestConnInputValidate(t *testing.T) {
	ctx := context.Background()
	strict := &Admin{}
	open := &Admin{AllowPrivateUpstreams: true}
	listed := &Admin{PrivateAllowlist: map[string]bool{"localhost": true}}
	base := connInput{Name: "n", ProviderType: "custom_openai", ModelName: "gpt-4o", BaseURL: "https://api.example.com/v1"}
	with := func(f func(*connInput)) connInput { c := base; f(&c); return c }

	cases := []struct {
		name string
		a    *Admin
		in   connInput
		want string // substring of the error; "" means valid
	}{
		{"ok", strict, base, ""},
		{"no scheme", strict, with(func(c *connInput) { c.BaseURL = "api.example.com" }), "http:// or https://"},
		{"ftp", strict, with(func(c *connInput) { c.BaseURL = "ftp://api.example.com" }), "http:// or https://"},
		{"userinfo", strict, with(func(c *connInput) { c.BaseURL = "https://u:p@api.example.com" }), "credentials"},
		{"query", strict, with(func(c *connInput) { c.BaseURL = "https://api.example.com/v1?x=1" }), "query string"},
		{"fragment", strict, with(func(c *connInput) { c.BaseURL = "https://api.example.com/v1#f" }), "query string"},
		{"no host", strict, with(func(c *connInput) { c.BaseURL = "http:///v1" }), "host"},
		{"loopback literal", strict, with(func(c *connInput) { c.BaseURL = "http://127.0.0.1:11434" }), "PRIVATE_UPSTREAM_ALLOWLIST"},
		{"metadata", strict, with(func(c *connInput) { c.BaseURL = "http://169.254.169.254/latest" }), "private or local"},
		{"localhost name", strict, with(func(c *connInput) { c.BaseURL = "http://localhost:11434" }), "private or local"},
		{"localhost allowlisted", listed, with(func(c *connInput) { c.BaseURL = "http://LOCALHOST:11434" }), ""},
		{"loopback allowed globally", open, with(func(c *connInput) { c.BaseURL = "http://127.0.0.1:11434" }), ""},
		{"empty base_url for openai", strict, with(func(c *connInput) { c.ProviderType = "openai"; c.BaseURL = "" }), ""},
		{"empty base_url for custom", strict, with(func(c *connInput) { c.BaseURL = "" }), "required"},
		{"model slash prefix", strict, with(func(c *connInput) { c.ModelName = "/x" }), "model_name"},
		{"model dotdot", strict, with(func(c *connInput) { c.ModelName = "a/../b" }), "model_name"},
		{"model space", strict, with(func(c *connInput) { c.ModelName = "a b" }), "model_name"},
		{"model too long", strict, with(func(c *connInput) { c.ModelName = strings.Repeat("a", 129) }), "model_name"},
		{"model ok", strict, with(func(c *connInput) { c.ModelName = "org/model-1.5:latest@v2_x" }), ""},
	}
	for _, c := range cases {
		err := c.in.validate(ctx, c.a)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: unexpected error %v", c.name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
}

func TestEffectiveLimitAndStoreQuota(t *testing.T) {
	cases := []struct{ store, instance, want int64 }{
		{0, 0, 0}, {10, 0, 10}, {0, 10, 10}, {5, 10, 5}, {10, 5, 5},
	}
	for _, c := range cases {
		if got := effectiveLimit(c.store, c.instance); got != c.want {
			t.Errorf("effectiveLimit(%d, %d) = %d, want %d", c.store, c.instance, got, c.want)
		}
	}
	q := &storeQuota{maxDocs: 2, docs: 1, maxBytes: 100, bytes: 60}
	if err := q.add(30); err != nil {
		t.Fatalf("first file should fit: %v", err)
	}
	if err := q.add(5); err == nil || !strings.Contains(err.Error(), "2 of 2 documents") {
		t.Errorf("document limit: %v", err)
	}
	q = &storeQuota{maxBytes: 100, bytes: 60}
	if err := q.add(41); err == nil || !strings.Contains(err.Error(), "60 of 100 bytes") {
		t.Errorf("byte limit: %v", err)
	}
	if err := (&storeQuota{}).add(1 << 40); err != nil {
		t.Errorf("unlimited: %v", err)
	}
}

func TestCSVRecordEscapesFormulas(t *testing.T) {
	for in, want := range map[string]string{
		"": "", "gpt-4o": "gpt-4o", "=cmd|' /C calc'!A0": "'=cmd|' /C calc'!A0", "+1": "'+1", "-1": "'-1",
		"@SUM(A1)": "'@SUM(A1)", "\t=1": "'\t=1", "\r=1": "'\r=1", "plain -dash": "plain -dash",
	} {
		if got := csvCell(in); got != want {
			t.Errorf("csvCell(%q) = %q, want %q", in, got, want)
		}
	}
	row := &store.RequestExportRow{ProjectName: "=HYPERLINK(\"x\")"}
	row.CreatedAt, row.ProjectID, row.ModelName, row.StatusCode = "2026-09-18T10:00:00Z", 3, "m", 200
	row.PromptTokens, row.CompletionTokens, row.LatencyMs, row.Streamed, row.RAGUsed, row.RAGHits = 10, 2, 45, true, true, 2
	row.Error = "-DDE"
	row.CachedPromptTokens, row.CacheWriteTokens, row.CostUSD, row.CostSource = 6, 4, 0.001234, "builtin"
	keyID := int64(7)
	row.APIKeyID = &keyID
	row.SessionID, row.ToolNames, row.ToolsDenied = "run-1", []string{"=cmd", "get_weather"}, 2
	got := csvRecord(row)
	// user_id stays empty: a request made with a project's default key has no
	// owner, and an empty cell says that where a 0 would read as user zero.
	want := []string{"2026-09-18T10:00:00Z", "3", "'=HYPERLINK(\"x\")", "m", "200", "10", "2", "false", "45", "true", "true", "2", "'-DDE",
		"6", "4", "0.001234", "builtin", "7", "", "run-1", "'=cmd;get_weather", "2"}
	if len(got) != len(csvHeader) || strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("record = %q\nwant     %q", got, want)
	}
}

func TestUpstreamPrivateLiterals(t *testing.T) {
	ctx := context.Background()
	for baseURL, want := range map[string]bool{
		"":                          false, // provider default URL
		"http://localhost:11434":    true,
		"http://127.0.0.1:8000/v1":  true,
		"http://[::1]:8000/v1":      true,
		"http://172.16.5.5/v1":      true,
		"http://169.254.169.254/v1": true,
		"https://8.8.8.8/v1":        false,
	} {
		got := upstreamPrivate(ctx, baseURL)
		if got == nil || *got != want {
			t.Errorf("upstreamPrivate(%q) = %v, want %v", baseURL, got, want)
		}
	}
	if got := upstreamPrivate(ctx, "://bad"); got != nil {
		t.Errorf("unparseable url should be unknown, got %v", *got)
	}
}

// ---- guardrails (ADR-034) ----

func TestProjectGuardrailsAPI(t *testing.T) {
	e := newAdminEnv(t)
	editor := e.user("editor", "editor", e.proj.ID)
	viewer := e.user("viewer", "viewer", e.proj.ID)
	outsider := e.user("outsider", "editor")
	path := "/api/projects/" + itoa(e.proj.ID) + "/guardrails"
	rule := func(id, typ, action string, stages ...string) map[string]any {
		return map[string]any{"id": id, "type": typ, "action": action, "stages": stages}
	}

	// A project starts with no rules and the defaults in force.
	status, out := e.do(http.MethodGet, path, nil, viewer.token)
	if status != http.StatusOK || len(out["rules"].([]any)) != 0 || out["stream_window_chars"] != float64(128) {
		t.Fatalf("empty GET: %d %v", status, out)
	}
	if wh := out["webhook"].(map[string]any); wh["enabled"] != false || wh["timeout_ms"] != float64(2000) || wh["failure_mode"] != "fail_closed" {
		t.Errorf("webhook defaults = %v", wh)
	}

	body := map[string]any{"rules": []any{
		rule("tckn", "tckn", "redact", "pre_request", "post_response"),
		rule("iban", "iban", "block", "pre_request"),
		map[string]any{"id": "words", "type": "keywords", "action": "flag", "stages": []string{"pre_retrieval"}, "keywords": []string{"gizli"}},
	}, "stream_window_chars": 48}
	status, out = e.do(http.MethodPut, path, body, editor.token)
	if status != http.StatusOK || len(out["rules"].([]any)) != 3 || out["stream_window_chars"] != float64(48) {
		t.Fatalf("PUT: %d %v", status, out)
	}
	if w := out["warnings"].([]any); len(w) != 1 || !strings.Contains(w[0].(string), "stream_window_chars") {
		t.Errorf("a tckn rule on post_response with W=48 must warn, got %v", w)
	}
	// What was stored is what the gateway reads.
	p, err := e.st.GetProject(context.Background(), e.proj.ID)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := guardrail.Parse(p.Guardrails)
	if err != nil || len(cfg.Rules) != 3 || cfg.Window() != 48 {
		t.Fatalf("stored %s: %v", p.Guardrails, err)
	}
	if _, err := guardrail.Compile(cfg); err != nil {
		t.Errorf("stored configuration does not compile: %v", err)
	}
	// A viewer reads it, an outsider does not know it exists, a viewer cannot write.
	if status, got := e.do(http.MethodGet, path, nil, viewer.token); status != http.StatusOK || len(got["rules"].([]any)) != 3 {
		t.Errorf("viewer GET: %d %v", status, got)
	}
	if status, _ := e.do(http.MethodGet, path, nil, outsider.token); status != http.StatusNotFound {
		t.Errorf("outsider GET: %d", status)
	}
	if status, _ := e.do(http.MethodPut, path, body, outsider.token); status != http.StatusNotFound {
		t.Errorf("outsider PUT: %d", status)
	}
	if status, _ := e.do(http.MethodPut, path, body, viewer.token); status != http.StatusForbidden {
		t.Errorf("viewer PUT: %d, want 403", status)
	}

	// An ordinary project update leaves the rules alone.
	status, out = e.do(http.MethodPut, "/api/projects/"+itoa(e.proj.ID), map[string]any{
		"name": "prod", "model_connection_id": e.conn.ID, "system_prompt": "hi"}, editor.token)
	if status != http.StatusOK {
		t.Fatalf("project update: %d %v", status, out)
	}
	if _, has := out["guardrails"]; has {
		t.Error("the project JSON must not carry the guardrails; they have their own route")
	}
	if status, got := e.do(http.MethodGet, path, nil, editor.token); status != http.StatusOK || len(got["rules"].([]any)) != 3 {
		t.Errorf("the rules did not survive a project update: %d %v", status, got)
	}

	// Refusals say which rule is wrong.
	long := strings.Repeat("a", 513)
	for name, tc := range map[string]struct {
		body map[string]any
		want string
	}{
		"bad id":          {map[string]any{"rules": []any{rule("Bad Id", "tckn", "flag", "pre_request")}}, "id must match"},
		"duplicate id":    {map[string]any{"rules": []any{rule("a", "tckn", "flag", "pre_request"), rule("a", "iban", "flag", "pre_request")}}, `"a" is used twice`},
		"unknown type":    {map[string]any{"rules": []any{rule("a", "email", "flag", "pre_request")}}, "unknown type"},
		"unknown action":  {map[string]any{"rules": []any{rule("a", "tckn", "drop", "pre_request")}}, "action must be"},
		"unknown stage":   {map[string]any{"rules": []any{rule("a", "tckn", "flag", "later")}}, "unknown stage"},
		"no stage":        {map[string]any{"rules": []any{rule("a", "tckn", "flag")}}, "stages must name"},
		"bad regex":       {map[string]any{"rules": []any{map[string]any{"id": "r", "type": "regex", "action": "flag", "stages": []string{"pre_request"}, "pattern": "("}}}, "does not compile"},
		"long regex":      {map[string]any{"rules": []any{map[string]any{"id": "r", "type": "regex", "action": "flag", "stages": []string{"pre_request"}, "pattern": long}}}, "512 bytes"},
		"window too low":  {map[string]any{"rules": []any{}, "stream_window_chars": 8}, "stream_window_chars"},
		"timeout too low": {map[string]any{"rules": []any{}, "webhook": map[string]any{"enabled": true, "timeout_ms": 10}}, "timeout_ms"},
		"failure mode":    {map[string]any{"rules": []any{}, "webhook": map[string]any{"enabled": true, "failure_mode": "maybe"}}, "failure_mode"},
		"unknown field":   {map[string]any{"rules": []any{}, "instance_default": true}, "invalid body"},
	} {
		status, out := e.do(http.MethodPut, path, tc.body, editor.token)
		if status != http.StatusBadRequest || !strings.Contains(errMessage(out), tc.want) {
			t.Errorf("%s: %d %q, want 400 containing %q", name, status, errMessage(out), tc.want)
		}
	}
	if status, got := e.do(http.MethodGet, path, nil, editor.token); status != http.StatusOK || len(got["rules"].([]any)) != 3 {
		t.Errorf("a refused PUT must leave the stored rules alone: %d %v", status, got)
	}

	// An unbounded regex in a stream is allowed, with a warning.
	status, out = e.do(http.MethodPut, path, map[string]any{"rules": []any{
		map[string]any{"id": "r", "type": "regex", "action": "redact", "stages": []string{"post_response"}, "pattern": `secret-\w+`}}}, editor.token)
	if status != http.StatusOK || len(out["warnings"].([]any)) != 1 {
		t.Errorf("unbounded regex: %d %v", status, out)
	}

	// A webhook switched on before its endpoint exists warns; with one it does not.
	wh := map[string]any{"rules": []any{}, "webhook": map[string]any{"enabled": true, "timeout_ms": 500, "failure_mode": "fail_open"}}
	status, out = e.do(http.MethodPut, path, wh, editor.token)
	if status != http.StatusOK || len(out["warnings"].([]any)) != 1 || !strings.Contains(out["warnings"].([]any)[0].(string), "no guardrail webhook endpoint") {
		t.Errorf("webhook without an endpoint: %d %v", status, out)
	}
	if _, err := e.st.CreateWebhookEndpoint(context.Background(), &store.WebhookEndpoint{ProjectID: e.proj.ID,
		Purpose: store.WebhookPurposeGuardrail, URL: "https://hooks.example.com/guard", Secret: "whsec_" + strings.Repeat("QUJD", 11)}); err != nil {
		t.Fatal(err)
	}
	status, out = e.do(http.MethodPut, path, wh, editor.token)
	if status != http.StatusOK || len(out["warnings"].([]any)) != 0 {
		t.Errorf("webhook with an endpoint: %d %v", status, out)
	}
	if got := out["webhook"].(map[string]any); got["timeout_ms"] != float64(500) || got["failure_mode"] != "fail_open" || got["enabled"] != true {
		t.Errorf("webhook settings = %v", got)
	}

	// The audit trail knows, and holds no pattern.
	entries, _, err := e.st.ListAuditLogs(context.Background(), store.AuditFilter{Action: "project.guardrails_update"})
	if err != nil || len(entries) == 0 {
		t.Fatalf("audit entries: %v %v", entries, err)
	}
	for _, en := range entries {
		raw, _ := json.Marshal(en)
		if strings.Contains(string(raw), `secret-`) || strings.Contains(string(raw), "gizli") {
			t.Errorf("the audit trail holds a pattern or a word: %s", raw)
		}
	}
}

// A redact rule whose placeholder a block rule matches stalls any conversation
// that sends the redacted answer back: the API says so when the rules are saved.
func TestProjectGuardrailsWarnAboutAPlaceholderABlockRuleMatches(t *testing.T) {
	e := newAdminEnv(t)
	editor := e.user("editor", "editor", e.proj.ID)
	path := "/api/projects/" + itoa(e.proj.ID) + "/guardrails"
	body := map[string]any{"rules": []any{
		map[string]any{"id": "iban", "type": "tckn", "action": "redact", "stages": []string{"post_response"}},
		map[string]any{"id": "words", "type": "keywords", "action": "block", "stages": []string{"pre_request"}, "keywords": []string{"iban"}},
	}}
	status, out := e.do(http.MethodPut, path, body, editor.token)
	w, _ := out["warnings"].([]any)
	if status != http.StatusOK || len(w) != 1 || !strings.Contains(w[0].(string), `"words"`) || !strings.Contains(w[0].(string), `"iban"`) {
		t.Fatalf("PUT: %d %v", status, out)
	}
	// GET says the same.
	status, out = e.do(http.MethodGet, path, nil, editor.token)
	if w, _ := out["warnings"].([]any); status != http.StatusOK || len(w) != 1 {
		t.Errorf("GET: %d %v", status, out)
	}
	body["rules"].([]any)[1].(map[string]any)["keywords"] = []string{"secret"}
	if _, out = e.do(http.MethodPut, path, body, editor.token); len(out["warnings"].([]any)) != 0 {
		t.Errorf("no overlap, no warning: %v", out["warnings"])
	}
}
