package alerts

import (
	"context"
	"log/slog"

	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/internal/webhook"
)

// ActionEndpointDisabled is the audit action written when a receiver answers
// 410 Gone and its endpoint is closed.
const ActionEndpointDisabled = "webhook.endpoint_disabled"

// AuditStore is what the audited delivery store needs besides the worker's own
// store.
type AuditStore interface {
	webhook.DeliveryStore
	DisableWebhookEndpointAudited(ctx context.Context, id int64, entry *store.AuditLog) (bool, error)
}

// auditedDeliveries is the webhook worker's store with one addition: closing
// an endpoint is written to the audit trail (design W3). The worker lives in
// internal/webhook, which does not know the audit log; this wrapper is where
// the two meet, so the worker is unchanged. The closing and the audit row are
// one transaction in the store, written by the one call that closed the
// endpoint, so several deliveries (or replicas) hearing the same 410 at once
// leave one row, and an audit row never exists without the closing.
type auditedDeliveries struct {
	AuditStore
	log *slog.Logger
}

// NewAuditedDeliveries wraps st for webhook.NewWorker. DisableWebhookEndpoint
// then also writes an audit entry (actor "system", target the endpoint) unless
// the endpoint was already closed. The entry names the project and the purpose,
// never the URL or a secret: a webhook URL often carries a token. The
// endpoint's unsent alert events are forgotten with it, so the alerts a
// receiver missed reach it when the endpoint is opened again.
func NewAuditedDeliveries(st AuditStore, log *slog.Logger) webhook.DeliveryStore {
	if log == nil {
		log = slog.Default()
	}
	return &auditedDeliveries{AuditStore: st, log: log}
}

func (a *auditedDeliveries) DisableWebhookEndpoint(ctx context.Context, id int64) error {
	target := id
	_, err := a.DisableWebhookEndpointAudited(ctx, id, &store.AuditLog{ActorUsername: "system", Action: ActionEndpointDisabled,
		TargetType: "webhook_endpoint", TargetID: &target, Details: map[string]any{"reason": "receiver answered 410 Gone"}})
	return err
}
