package admin

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/ragmux/ragmux/internal/auth"
	"github.com/ragmux/ragmux/internal/guardrail"
	"github.com/ragmux/ragmux/internal/store"
)

// guardrailsBody is what GET and PUT /api/projects/{id}/guardrails exchange: the
// project's guardrail configuration (ADR-034), with every default filled in so
// the dashboard shows the value that is in force. Warnings are for what is
// allowed but risky; they are sent on PUT and GET, never stored.
type guardrailsBody struct {
	Rules             []guardrail.Rule  `json:"rules"`
	StreamWindowChars int               `json:"stream_window_chars"`
	Webhook           guardrail.Webhook `json:"webhook"`
	Warnings          []string          `json:"warnings"`
}

// guardrailsView normalises a configuration for the answer: rules never null,
// the window and the webhook settings at their defaults.
func guardrailsView(c guardrail.Config, warnings []string) guardrailsBody {
	out := guardrailsBody{Rules: c.Rules, StreamWindowChars: c.Window(), Warnings: warnings}
	if out.Rules == nil {
		out.Rules = []guardrail.Rule{}
	}
	if out.Warnings == nil {
		out.Warnings = []string{}
	}
	if c.Webhook != nil {
		out.Webhook = *c.Webhook
	}
	if out.Webhook.TimeoutMS == 0 {
		out.Webhook.TimeoutMS = guardrail.DefaultWebhookTimeoutMS
	}
	if out.Webhook.FailureMode == "" {
		out.Webhook.FailureMode = guardrail.FailClosed
	}
	return out
}

// config is the stored form of a body.
func (b guardrailsBody) config() guardrail.Config {
	wh := b.Webhook
	return guardrail.Config{Rules: b.Rules, StreamWindowChars: b.StreamWindowChars, Webhook: &wh}
}

// webhookWarning says what is wrong with a project that turned the webhook on
// before registering its endpoint. It is a warning, not a refusal: the endpoint
// is registered on its own route, and either order is a reasonable one.
const webhookWarning = "the webhook is enabled but the project has no guardrail webhook endpoint yet; " +
	"until one is registered every request is handled according to failure_mode"

// guardrailWarnings are the warnings Validate gives plus the ones that need the
// database.
func (a *Admin) guardrailWarnings(r *http.Request, projectID int64, c guardrail.Config, warnings []string) []string {
	if c.Webhook != nil && c.Webhook.Enabled {
		if _, err := a.Store.GetWebhookEndpoint(r.Context(), projectID, store.WebhookPurposeGuardrail); errors.Is(err, store.ErrNotFound) {
			warnings = append(warnings, webhookWarning)
		}
	}
	return warnings
}

// getGuardrails answers GET /api/projects/{id}/guardrails. The caller needs the
// access to the project that reading its settings takes.
func (a *Admin) getGuardrails(w http.ResponseWriter, r *http.Request) {
	p, ok := a.loadProject(w, r)
	if !ok {
		return
	}
	c, err := guardrail.Parse(p.Guardrails)
	if err != nil {
		// A stored value this build cannot read. The gateway refuses requests
		// for such a project; the dashboard has to be able to replace it.
		a.Log.Error("stored guardrail configuration is unreadable", "project", p.ID, "err", err)
		writeJSON(w, http.StatusOK, guardrailsView(guardrail.Config{}, []string{
			"the stored guardrail configuration cannot be read by this version; save a new one to replace it"}))
		return
	}
	warnings, verr := c.Validate()
	if verr != nil {
		warnings = []string{"the stored guardrail configuration is not valid for this version: " + verr.Error()}
	} else if set, err := guardrail.Compile(c); err == nil {
		warnings = append(warnings, set.PlaceholderWarnings()...)
	}
	writeJSON(w, http.StatusOK, guardrailsView(c, a.guardrailWarnings(r, p.ID, c, warnings)))
}

// putGuardrails answers PUT /api/projects/{id}/guardrails: it replaces the
// project's rules and settings as a whole. A rule that cannot work is a 400
// that says which one; one that can work but may miss in a stream is accepted
// with a warning.
func (a *Admin) putGuardrails(w http.ResponseWriter, r *http.Request) {
	p, ok := a.loadProject(w, r)
	if !ok {
		return
	}
	if !auth.IsBearer(r) && !isJSON(r) {
		badBody(w, errUnsupportedMediaType)
		return
	}
	var in guardrailsBody
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	cfg := in.config()
	set, err := guardrail.Compile(cfg)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	warnings, _ := cfg.Validate() // Compile validated it already
	warnings = append(warnings, set.PlaceholderWarnings()...)
	raw, err := json.Marshal(cfg)
	if err != nil {
		a.fail(w, err)
		return
	}
	if err := a.Store.SetProjectGuardrails(r.Context(), p.ID, raw); err != nil {
		a.fail(w, err)
		return
	}
	// The audit trail names the rules, never their patterns or words: those are
	// the operator's, but a pattern is often the thing it protects.
	ids := make([]string, 0, len(cfg.Rules))
	for _, rule := range cfg.Rules {
		ids = append(ids, rule.ID)
	}
	a.audit(r, "project.guardrails_update", "project", ptr(p.ID), map[string]any{"name": p.Name, "rules": ids,
		"stream_window_chars": cfg.Window(), "webhook_enabled": cfg.Webhook != nil && cfg.Webhook.Enabled})
	writeJSON(w, http.StatusOK, guardrailsView(cfg, a.guardrailWarnings(r, p.ID, cfg, warnings)))
}
