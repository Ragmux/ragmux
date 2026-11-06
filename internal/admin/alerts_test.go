package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/ragmux/ragmux/internal/store"
)

func hookPath(p *store.Project, purpose string) string {
	return "/api/projects/" + strconv.FormatInt(p.ID, 10) + "/webhooks/" + purpose
}

func alertsPath(p *store.Project) string {
	return "/api/projects/" + strconv.FormatInt(p.ID, 10) + "/alerts"
}

// The private-webhook allowlist is the only exemption a webhook URL gets: a
// host on it is accepted (over plain http too), any other private host is a
// 400, and the provider allowlist -- or allowing private upstreams outright --
// plays no part.
func TestWebhookRegistrationFollowsThePrivateWebhookAllowlist(t *testing.T) {
	e := newAdminEnv(t)
	e.adm.PrivateWebhookAllowlist = map[string]bool{"127.0.0.1": true}
	e.adm.PrivateAllowlist = map[string]bool{"10.5.5.5": true}
	e.adm.AllowPrivateUpstreams = true
	admin := e.user("admin", "admin")

	for _, purpose := range []string{"guardrail", "alerts"} {
		p := e.proj
		if purpose == "alerts" {
			p = e.other
		}
		status, out := e.do(http.MethodPut, hookPath(p, purpose), map[string]any{"url": "http://127.0.0.1:9/hook"}, admin.token)
		if status != http.StatusCreated {
			t.Errorf("%s, host on the webhook allowlist: %d %v", purpose, status, out)
		}
	}
	e2 := newAdminEnv(t)
	e2.adm.PrivateWebhookAllowlist = map[string]bool{"127.0.0.1": true}
	e2.adm.PrivateAllowlist = map[string]bool{"10.5.5.5": true}
	e2.adm.AllowPrivateUpstreams = true
	admin2 := e2.user("admin", "admin")
	for _, c := range []struct{ name, url string }{
		{"private host not listed (http)", "http://10.9.9.9/hook"},
		{"private host not listed (https)", "https://10.9.9.9/hook"},
		{"loopback not listed", "https://127.0.0.2/hook"},
		{"only on the provider allowlist (http)", "http://10.5.5.5/hook"},
		{"only on the provider allowlist (https)", "https://10.5.5.5/hook"},
		{"http to a public host", "http://8.8.8.8/hook"},
		{"credentials", "https://user:pw@8.8.8.8/hook"},
		{"scheme", "ftp://8.8.8.8/hook"},
		{"not a url", "not a url"},
		{"empty", ""},
		{"too long", "https://8.8.8.8/" + strings.Repeat("a", 2100)},
	} {
		status, out := e2.do(http.MethodPut, hookPath(e2.proj, "alerts"), map[string]any{"url": c.url}, admin2.token)
		if status != http.StatusBadRequest || errMessage(out) == "" {
			t.Errorf("%s: %d %v, want 400 with a message", c.name, status, out)
		}
	}
	if _, err := e2.st.GetWebhookEndpoint(context.Background(), e2.proj.ID, "alerts"); err == nil {
		t.Error("a refused URL left an endpoint behind")
	}
	// A public https host passes.
	if status, out := e2.do(http.MethodPut, hookPath(e2.proj, "alerts"), map[string]any{"url": "https://8.8.8.8/hook"}, admin2.token); status != http.StatusCreated {
		t.Errorf("public https: %d %v", status, out)
	}
}

func TestWebhookSecretIsShownOnceAndRotates(t *testing.T) {
	ctx := context.Background()
	e := newAdminEnv(t)
	admin := e.user("admin", "admin")
	path := hookPath(e.proj, "alerts")

	status, out := e.do(http.MethodPut, path, map[string]any{"url": "https://8.8.8.8/hook?token=abc123"}, admin.token)
	if status != http.StatusCreated {
		t.Fatalf("create: %d %v", status, out)
	}
	first, _ := out["secret"].(string)
	if !strings.HasPrefix(first, "whsec_") || out["purpose"] != "alerts" || out["disabled"] != false || out["previous_secret_expires_at"] != nil {
		t.Fatalf("create answer = %v", out)
	}
	if ep, err := e.st.GetWebhookEndpoint(ctx, e.proj.ID, "alerts"); err != nil || ep.Secret != first || ep.CreatedBy == nil || *ep.CreatedBy != admin.ID {
		t.Fatalf("stored endpoint = %+v, %v", ep, err)
	}

	// Reading and changing the URL never repeat it.
	status, out = e.do(http.MethodGet, path, nil, admin.token)
	if _, has := out["secret"]; status != http.StatusOK || has || out["url"] != "https://8.8.8.8/hook?token=abc123" {
		t.Errorf("get: %d %v", status, out)
	}
	status, out = e.do(http.MethodPut, path, map[string]any{"url": "https://8.8.4.4/hook"}, admin.token)
	if _, has := out["secret"]; status != http.StatusOK || has || out["url"] != "https://8.8.4.4/hook" {
		t.Errorf("second put: %d %v", status, out)
	}
	if ep, _ := e.st.GetWebhookEndpoint(ctx, e.proj.ID, "alerts"); ep.Secret != first {
		t.Error("changing the URL changed the secret")
	}

	status, out = e.do(http.MethodPost, path+"/rotate-secret", nil, admin.token)
	second, _ := out["secret"].(string)
	if status != http.StatusOK || !strings.HasPrefix(second, "whsec_") || second == first || out["previous_secret_expires_at"] == nil {
		t.Fatalf("rotate: %d %v", status, out)
	}
	ep, err := e.st.GetWebhookEndpoint(ctx, e.proj.ID, "alerts")
	if err != nil || ep.Secret != second || ep.PrevSecret != first || ep.PrevExpiresAt == nil {
		t.Fatalf("after rotation: %+v, %v", ep, err)
	}
	status, out = e.do(http.MethodGet, path, nil, admin.token)
	if _, has := out["secret"]; status != http.StatusOK || has || out["previous_secret_expires_at"] == nil {
		t.Errorf("get after rotation: %d %v", status, out)
	}

	// The trail names what happened but neither a URL nor a secret.
	logs, _, err := e.st.ListAuditLogs(ctx, store.AuditFilter{Action: "project.webhook"})
	if err != nil || len(logs) != 3 {
		t.Fatalf("%d audit rows, %v; want register, update, rotate", len(logs), err)
	}
	raw, _ := json.Marshal(logs)
	for _, leak := range []string{"abc123", "whsec_", "/hook", first, second} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("the audit trail contains %q: %s", leak, raw)
		}
	}

	// A 410 closed the endpoint; saving a URL opens it again.
	if err := e.st.DisableWebhookEndpoint(ctx, ep.ID); err != nil {
		t.Fatal(err)
	}
	if _, out = e.do(http.MethodGet, path, nil, admin.token); out["disabled"] != true {
		t.Errorf("closed endpoint reads %v", out)
	}
	if status, out = e.do(http.MethodPut, path, map[string]any{"url": "https://8.8.8.8/again"}, admin.token); status != http.StatusOK || out["disabled"] != false {
		t.Errorf("reopen: %d %v", status, out)
	}

	status, _ = e.do(http.MethodDelete, path, nil, admin.token)
	if status != http.StatusNoContent {
		t.Errorf("delete: %d", status)
	}
	if status, _ = e.do(http.MethodGet, path, nil, admin.token); status != http.StatusNotFound {
		t.Errorf("get after delete: %d", status)
	}
	if status, _ = e.do(http.MethodPost, path+"/rotate-secret", nil, admin.token); status != http.StatusNotFound {
		t.Errorf("rotate after delete: %d", status)
	}
}

func TestWebhookEndpointsAreForEditorsOfTheProject(t *testing.T) {
	e := newAdminEnv(t)
	viewer := e.user("viewer", "viewer", e.proj.ID)
	editor := e.user("editor", "editor", e.proj.ID)
	outsider := e.user("outsider", "editor") // an editor, but not a member
	path := hookPath(e.proj, "alerts")
	body := map[string]any{"url": "https://8.8.8.8/hook"}

	if status, _ := e.do(http.MethodPut, path, body, viewer.token); status != http.StatusForbidden {
		t.Errorf("viewer put: %d", status)
	}
	if status, _ := e.do(http.MethodPut, path, body, outsider.token); status != http.StatusNotFound {
		t.Errorf("non-member put: %d", status)
	}
	if status, _ := e.do(http.MethodPut, path, body, editor.token); status != http.StatusCreated {
		t.Fatalf("editor put: %d", status)
	}
	// The URL may hold a token: a viewer does not read it either.
	if status, out := e.do(http.MethodGet, path, nil, viewer.token); status != http.StatusForbidden {
		t.Errorf("viewer get: %d %v", status, out)
	}
	if status, _ := e.do(http.MethodGet, path, nil, outsider.token); status != http.StatusNotFound {
		t.Errorf("non-member get: %d", status)
	}
	for _, c := range []struct{ method, path string }{
		{http.MethodDelete, path}, {http.MethodPost, path + "/rotate-secret"},
	} {
		if status, _ := e.do(c.method, c.path, nil, viewer.token); status != http.StatusForbidden {
			t.Errorf("viewer %s %s: %d", c.method, c.path, status)
		}
		if status, _ := e.do(c.method, c.path, nil, outsider.token); status != http.StatusNotFound {
			t.Errorf("non-member %s %s: %d", c.method, c.path, status)
		}
	}
	if status, _ := e.do(http.MethodPut, hookPath(e.proj, "billing"), body, editor.token); status != http.StatusNotFound {
		t.Errorf("unknown purpose: %d", status)
	}
	if status, _ := e.do(http.MethodPut, path, map[string]any{"url": "https://8.8.8.8/x", "secret": "whsec_mine"}, editor.token); status != http.StatusBadRequest {
		t.Errorf("a field the API does not take: %d", status)
	}
}

func TestAlertsSettings(t *testing.T) {
	ctx := context.Background()
	e := newAdminEnv(t)
	viewer := e.user("viewer", "viewer", e.proj.ID)
	editor := e.user("editor", "editor", e.proj.ID)
	outsider := e.user("outsider", "viewer")
	path := alertsPath(e.proj)

	status, out := e.do(http.MethodGet, path, nil, viewer.token)
	if status != http.StatusOK || out["enabled"] != false || out["budget_monthly_usd_micros"] != float64(0) || out["updated_at"] != nil {
		t.Fatalf("defaults: %d %v", status, out)
	}
	if wh, _ := out["webhook"].(map[string]any); wh["registered"] != false {
		t.Errorf("webhook = %v", out["webhook"])
	}
	if status, _ = e.do(http.MethodGet, path, nil, outsider.token); status != http.StatusNotFound {
		t.Errorf("non-member get: %d", status)
	}
	if status, _ = e.do(http.MethodPut, path, map[string]any{"enabled": true}, viewer.token); status != http.StatusForbidden {
		t.Errorf("viewer put: %d", status)
	}

	// On without an endpoint: allowed, with a warning that nothing will be sent.
	status, out = e.do(http.MethodPut, path, map[string]any{"enabled": true, "budget_monthly_usd_micros": 25_000_000}, editor.token)
	if status != http.StatusOK || out["enabled"] != true || out["budget_monthly_usd_micros"] != float64(25_000_000) || out["updated_at"] == nil {
		t.Fatalf("put: %d %v", status, out)
	}
	if w, _ := out["warnings"].([]any); len(w) != 1 || !strings.Contains(w[0].(string), "no alerts webhook endpoint") {
		t.Errorf("warnings = %v", out["warnings"])
	}
	if cfg, err := e.st.GetAlertSettings(ctx, e.proj.ID); err != nil || !cfg.Enabled || cfg.BudgetMonthlyUSDMicros != 25_000_000 {
		t.Errorf("stored = %+v, %v", cfg, err)
	}

	if status, _ = e.do(http.MethodPut, hookPath(e.proj, "alerts"), map[string]any{"url": "https://8.8.8.8/hook"}, editor.token); status != http.StatusCreated {
		t.Fatalf("register: %d", status)
	}
	status, out = e.do(http.MethodGet, path, nil, viewer.token)
	if w, _ := out["warnings"].([]any); status != http.StatusOK || len(w) != 0 {
		t.Errorf("with an endpoint: %d %v", status, out)
	}
	if wh, _ := out["webhook"].(map[string]any); wh["registered"] != true || wh["disabled"] != false {
		t.Errorf("webhook = %v", out["webhook"])
	}
	// The view never shows the URL: a viewer reads it.
	if raw, _ := json.Marshal(out); strings.Contains(string(raw), "8.8.8.8") {
		t.Errorf("the alerts view shows the endpoint URL: %s", raw)
	}

	// A budget with the alerts off warns nobody; say so.
	status, out = e.do(http.MethodPut, path, map[string]any{"enabled": false, "budget_monthly_usd_micros": 1}, editor.token)
	if w, _ := out["warnings"].([]any); status != http.StatusOK || len(w) != 1 {
		t.Errorf("off with a budget: %d %v", status, out)
	}

	for _, c := range []struct {
		name string
		body any
	}{
		{"negative budget", map[string]any{"enabled": true, "budget_monthly_usd_micros": -1}},
		{"budget beyond a JSON-safe integer", map[string]any{"enabled": true, "budget_monthly_usd_micros": int64(1) << 60}},
		{"a field the API does not take", map[string]any{"enabled": true, "threshold": 50}},
		{"a string budget", map[string]any{"enabled": true, "budget_monthly_usd_micros": "10"}},
	} {
		if status, out := e.do(http.MethodPut, path, c.body, editor.token); status != http.StatusBadRequest {
			t.Errorf("%s: %d %v", c.name, status, out)
		}
	}
	logs, _, err := e.st.ListAuditLogs(ctx, store.AuditFilter{Action: "project.alerts_update"})
	if err != nil || len(logs) != 2 {
		t.Errorf("%d audit rows, %v; want the two accepted updates", len(logs), err)
	}
}

// An endpoint whose secret this replica cannot open (a damaged row, a replica
// with the wrong SECRET_KEY) must stay manageable: showing it, pointing it
// elsewhere, rotating its secret and deleting it need no secret, and rotating
// is the documented way out of a lost one.
func TestWebhookWithUnreadableSecretStaysManageable(t *testing.T) {
	ctx := context.Background()
	e := newAdminEnv(t)
	admin := e.user("admin", "admin")
	path := hookPath(e.proj, "alerts")
	if status, out := e.do(http.MethodPut, path, map[string]any{"url": "https://8.8.8.8/hook"}, admin.token); status != http.StatusCreated {
		t.Fatalf("create: %d %v", status, out)
	}
	damage := func() {
		t.Helper()
		if _, err := e.st.DB().Exec(ctx, "UPDATE webhook_endpoints SET secret_enc = '\\x0001'::bytea WHERE project_id = $1", e.proj.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := e.st.GetWebhookEndpoint(ctx, e.proj.ID, "alerts"); err == nil {
			t.Fatal("the damaged secret opens; the test would prove nothing")
		}
	}

	damage()
	if status, out := e.do(http.MethodGet, alertsPath(e.proj), nil, admin.token); status != http.StatusOK {
		t.Errorf("alerts view: %d %v", status, out)
	} else if wh, _ := out["webhook"].(map[string]any); wh["registered"] != true {
		t.Errorf("alerts view webhook = %v", wh)
	}
	if status, out := e.do(http.MethodGet, path, nil, admin.token); status != http.StatusOK || out["url"] != "https://8.8.8.8/hook" {
		t.Errorf("get: %d %v", status, out)
	}
	if status, out := e.do(http.MethodPut, path, map[string]any{"url": "https://8.8.4.4/hook"}, admin.token); status != http.StatusOK || out["url"] != "https://8.8.4.4/hook" {
		t.Errorf("put: %d %v", status, out)
	}
	status, out := e.do(http.MethodPost, path+"/rotate-secret", nil, admin.token)
	secret, _ := out["secret"].(string)
	if status != http.StatusOK || !strings.HasPrefix(secret, "whsec_") {
		t.Fatalf("rotate: %d %v", status, out)
	}
	if ep, err := e.st.GetWebhookEndpoint(ctx, e.proj.ID, "alerts"); err != nil || ep.Secret != secret {
		t.Errorf("after rotating the secret opens again: %+v, %v", ep, err)
	}

	damage()
	if status, _ := e.do(http.MethodDelete, path, nil, admin.token); status != http.StatusNoContent {
		t.Errorf("delete: %d", status)
	}
}
