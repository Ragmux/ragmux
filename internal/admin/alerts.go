package admin

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/ragmux/ragmux/internal/auth"
	"github.com/ragmux/ragmux/internal/store"
)

// maxAlertBudgetMicros keeps the money budget inside what a JSON number
// carries without losing digits in a browser (2^53 - 1 millionths of a dollar,
// about nine billion dollars).
const maxAlertBudgetMicros = 1<<53 - 1

// alertsInput is what PUT /api/projects/{id}/alerts takes: the project's whole
// alert configuration (ADR-036). The thresholds are fixed (80 % and 100 %) and
// there is nothing else to set.
type alertsInput struct {
	Enabled bool `json:"enabled"`
	// BudgetMonthlyUSDMicros is the monthly money budget in USD millionths
	// (1 000 000 = one dollar); 0 sets none. It only warns: no request is
	// ever refused for exceeding it.
	BudgetMonthlyUSDMicros int64 `json:"budget_monthly_usd_micros"`
}

// alertsWebhook says whether the alerts endpoint is in place, without showing
// its URL: a webhook URL often carries a token, and this view is open to
// viewers.
type alertsWebhook struct {
	Registered bool `json:"registered"`
	Disabled   bool `json:"disabled"`
}

// alertsView is what GET and PUT answer.
type alertsView struct {
	alertsInput
	UpdatedAt *string       `json:"updated_at"`
	Webhook   alertsWebhook `json:"webhook"`
	Warnings  []string      `json:"warnings"`
}

// buildAlertsView reads the project's configuration and the state of its endpoint
// and lists what would keep an alert from going out.
func (a *Admin) buildAlertsView(r *http.Request, projectID int64, cfg store.AlertSettings) (alertsView, error) {
	out := alertsView{alertsInput: alertsInput{Enabled: cfg.Enabled, BudgetMonthlyUSDMicros: cfg.BudgetMonthlyUSDMicros},
		Warnings: []string{}}
	if !cfg.UpdatedAt.IsZero() {
		s := cfg.UpdatedAt.UTC().Format(time.RFC3339)
		out.UpdatedAt = &s
	}
	ep, err := a.Store.GetWebhookEndpointMeta(r.Context(), projectID, store.WebhookPurposeAlerts)
	switch {
	case err == nil:
		out.Webhook = alertsWebhook{Registered: true, Disabled: ep.DisabledAt != nil}
	case errors.Is(err, store.ErrNotFound):
	default:
		return out, err
	}
	if cfg.Enabled && !out.Webhook.Registered {
		out.Warnings = append(out.Warnings, "alerts are on but the project has no alerts webhook endpoint yet; nothing is sent until one is registered")
	}
	if cfg.Enabled && out.Webhook.Disabled {
		out.Warnings = append(out.Warnings, "the alerts webhook endpoint was closed because the receiver answered 410 Gone; register its URL again to resume")
	}
	if !cfg.Enabled && cfg.BudgetMonthlyUSDMicros > 0 {
		out.Warnings = append(out.Warnings, "a monthly money budget is set but alerts are off, so it warns nobody")
	}
	return out, nil
}

// getAlerts answers GET /api/projects/{id}/alerts.
func (a *Admin) getAlerts(w http.ResponseWriter, r *http.Request) {
	p, ok := a.loadProject(w, r)
	if !ok {
		return
	}
	cfg, err := a.Store.GetAlertSettings(r.Context(), p.ID)
	if err != nil {
		a.fail(w, err)
		return
	}
	view, err := a.buildAlertsView(r, p.ID, cfg)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// putAlerts answers PUT /api/projects/{id}/alerts: it replaces the project's
// alert configuration as a whole.
func (a *Admin) putAlerts(w http.ResponseWriter, r *http.Request) {
	p, ok := a.loadProject(w, r)
	if !ok {
		return
	}
	if !auth.IsBearer(r) && !isJSON(r) {
		badBody(w, errUnsupportedMediaType)
		return
	}
	var in alertsInput
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if in.BudgetMonthlyUSDMicros < 0 || in.BudgetMonthlyUSDMicros > maxAlertBudgetMicros {
		writeErr(w, http.StatusBadRequest, "budget_monthly_usd_micros must be between 0 and 9007199254740991")
		return
	}
	cfg, err := a.Store.SetAlertSettings(r.Context(), p.ID, in.Enabled, in.BudgetMonthlyUSDMicros)
	if err != nil {
		a.fail(w, err)
		return
	}
	a.audit(r, "project.alerts_update", "project", ptr(p.ID), map[string]any{"name": p.Name,
		"enabled": cfg.Enabled, "budget_monthly_usd_micros": cfg.BudgetMonthlyUSDMicros})
	view, err := a.buildAlertsView(r, p.ID, cfg)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}
