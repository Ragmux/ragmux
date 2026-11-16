package admin

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ragmux/ragmux/internal/auth"
	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/internal/webhook"
)

// webhookInput is what PUT /api/projects/{id}/webhooks/{purpose} takes.
type webhookInput struct {
	URL string `json:"url"`
}

// webhookView is an endpoint as the admin API shows it. The secret is not part
// of it: it is sent once, by webhookSecretView, when it is created or rotated.
type webhookView struct {
	Purpose   string `json:"purpose"`
	URL       string `json:"url"`
	Disabled  bool   `json:"disabled"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	// PreviousSecretExpiresAt is set while a rotated-out secret still signs
	// next to the new one.
	PreviousSecretExpiresAt *string `json:"previous_secret_expires_at"`
}

// webhookSecretView is webhookView plus the signing secret. The secret is
// stored sealed and is shown here and nowhere else, so a lost one is replaced
// by rotating it.
type webhookSecretView struct {
	webhookView
	Secret string `json:"secret"`
}

func newWebhookView(e store.WebhookEndpointMeta, now time.Time) webhookView {
	v := webhookView{Purpose: e.Purpose, URL: e.URL, Disabled: e.DisabledAt != nil,
		CreatedAt: e.CreatedAt.UTC().Format(time.RFC3339), UpdatedAt: e.UpdatedAt.UTC().Format(time.RFC3339)}
	if e.PrevExpiresAt != nil && e.PrevExpiresAt.After(now) {
		s := e.PrevExpiresAt.UTC().Format(time.RFC3339)
		v.PreviousSecretExpiresAt = &s
	}
	return v
}

// webhookPurpose reads {purpose} from the path; anything but the two known
// purposes is a 404.
func webhookPurpose(w http.ResponseWriter, r *http.Request) (string, bool) {
	switch p := chi.URLParam(r, "purpose"); p {
	case store.WebhookPurposeGuardrail, store.WebhookPurposeAlerts:
		return p, true
	}
	writeErr(w, http.StatusNotFound, "not found")
	return "", false
}

// validateWebhookURL applies the webhook URL rules: https (http only for a host
// on PRIVATE_WEBHOOK_ALLOWLIST), no credentials, at most 2048 bytes, and a
// host that does not resolve only to private or local addresses unless
// PRIVATE_WEBHOOK_ALLOWLIST names it. PRIVATE_UPSTREAM_ALLOWLIST plays no part:
// a provider exemption must not reach a URL an editor types here (W7). The
// dialer checks again on every connection.
func (a *Admin) validateWebhookURL(r *http.Request, raw string) error {
	return webhook.NewClient(a.PrivateWebhookAllowlist).ValidateURL(r.Context(), raw)
}

// webhookHost is the part of a webhook URL that is safe to write to the audit
// trail: the rest often carries a token.
func webhookHost(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return u.Host
	}
	return ""
}

// getWebhook answers GET /api/projects/{id}/webhooks/{purpose}. It is for
// editors: the URL is part of the answer and may hold a token.
func (a *Admin) getWebhook(w http.ResponseWriter, r *http.Request) {
	p, ok := a.loadProject(w, r)
	if !ok {
		return
	}
	purpose, ok := webhookPurpose(w, r)
	if !ok {
		return
	}
	ep, err := a.Store.GetWebhookEndpointMeta(r.Context(), p.ID, purpose)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, newWebhookView(*ep, time.Now()))
}

// putWebhook answers PUT /api/projects/{id}/webhooks/{purpose}. The first call
// registers the endpoint, generates its secret and answers 201 with it, once;
// a later call changes the URL, keeps the secret, opens an endpoint that was
// closed by a 410 and answers 200 without it.
func (a *Admin) putWebhook(w http.ResponseWriter, r *http.Request) {
	p, ok := a.loadProject(w, r)
	if !ok {
		return
	}
	purpose, ok := webhookPurpose(w, r)
	if !ok {
		return
	}
	if !auth.IsBearer(r) && !isJSON(r) {
		badBody(w, errUnsupportedMediaType)
		return
	}
	var in webhookInput
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	in.URL = strings.TrimSpace(in.URL)
	if err := a.validateWebhookURL(r, in.URL); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// Read without the secret: an endpoint whose secret this replica cannot
	// open must still be re-pointable, or the operator has no way out.
	existing, err := a.Store.GetWebhookEndpointMeta(r.Context(), p.ID, purpose)
	switch {
	case err == nil:
		ep, err := a.Store.SetWebhookEndpointURL(r.Context(), existing.ID, in.URL)
		if err != nil {
			a.fail(w, err)
			return
		}
		a.audit(r, "project.webhook_update", "project", ptr(p.ID), map[string]any{"name": p.Name,
			"purpose": purpose, "host": webhookHost(in.URL), "reopened": existing.DisabledAt != nil})
		writeJSON(w, http.StatusOK, newWebhookView(*ep, time.Now()))
		return
	case !errors.Is(err, store.ErrNotFound):
		a.fail(w, err)
		return
	}
	secret, err := webhook.GenerateSecret()
	if err != nil {
		a.fail(w, err)
		return
	}
	e := &store.WebhookEndpoint{ProjectID: p.ID, Purpose: purpose, URL: in.URL, Secret: secret}
	if u := auth.UserFrom(r.Context()); u != nil {
		id := u.ID
		e.CreatedBy = &id
	}
	ep, err := a.Store.CreateWebhookEndpoint(r.Context(), e)
	if store.IsUniqueViolation(err) {
		// Registered by someone else between the lookup and the insert.
		writeErr(w, http.StatusConflict, "this project already has a "+purpose+" webhook endpoint; send the request again to change its URL")
		return
	}
	if err != nil {
		a.fail(w, err)
		return
	}
	a.audit(r, "project.webhook_register", "project", ptr(p.ID), map[string]any{"name": p.Name,
		"purpose": purpose, "host": webhookHost(in.URL)})
	writeJSON(w, http.StatusCreated, webhookSecretView{webhookView: newWebhookView(ep.Meta(), time.Now()), Secret: secret})
}

// deleteWebhook answers DELETE /api/projects/{id}/webhooks/{purpose}: the
// endpoint and the deliveries still queued for it go.
func (a *Admin) deleteWebhook(w http.ResponseWriter, r *http.Request) {
	p, ok := a.loadProject(w, r)
	if !ok {
		return
	}
	purpose, ok := webhookPurpose(w, r)
	if !ok {
		return
	}
	existing, err := a.Store.GetWebhookEndpointMeta(r.Context(), p.ID, purpose)
	if err != nil {
		a.fail(w, err)
		return
	}
	// The alerts it had not delivered are forgotten with it, so registering a
	// new endpoint brings a limit that is still crossed back to its receiver.
	if err := a.Store.DeleteWebhookEndpointForgettingUnsent(r.Context(), existing.ID); err != nil {
		a.fail(w, err)
		return
	}
	a.audit(r, "project.webhook_delete", "project", ptr(p.ID), map[string]any{"name": p.Name, "purpose": purpose})
	w.WriteHeader(http.StatusNoContent)
}

// rotateWebhookSecret answers POST /api/projects/{id}/webhooks/{purpose}/rotate-secret.
// The new secret is in the answer, once. The old one keeps signing next to it
// for webhook.RotationOverlap so the receiver can switch without dropping a
// delivery.
func (a *Admin) rotateWebhookSecret(w http.ResponseWriter, r *http.Request) {
	p, ok := a.loadProject(w, r)
	if !ok {
		return
	}
	purpose, ok := webhookPurpose(w, r)
	if !ok {
		return
	}
	existing, err := a.Store.GetWebhookEndpointMeta(r.Context(), p.ID, purpose)
	if err != nil {
		a.fail(w, err)
		return
	}
	secret, err := webhook.GenerateSecret()
	if err != nil {
		a.fail(w, err)
		return
	}
	ep, err := a.Store.RotateWebhookSecret(r.Context(), existing.ID, secret, webhook.RotationOverlap)
	if err != nil {
		a.fail(w, err)
		return
	}
	a.audit(r, "project.webhook_rotate_secret", "project", ptr(p.ID), map[string]any{"name": p.Name, "purpose": purpose})
	writeJSON(w, http.StatusOK, webhookSecretView{webhookView: newWebhookView(ep.Meta(), time.Now()), Secret: secret})
}
