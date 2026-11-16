package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// webhookAADPrefix is the associated-data prefix of webhook_endpoints.secret_enc
// and prev_secret_enc (ADR-033); the endpoint id is appended.
const webhookAADPrefix = "ragmux:webhook_endpoint:"

// Webhook endpoint purposes: one endpoint per project and purpose.
const (
	WebhookPurposeGuardrail = "guardrail"
	WebhookPurposeAlerts    = "alerts"
)

// Delivery states of webhook_deliveries.status.
const (
	DeliveryPending   = "pending"
	DeliveryDelivered = "delivered"
	DeliveryFailed    = "failed"
)

// Delivery error classes of webhook_deliveries.last_error_class. The set is
// closed on purpose: it is stored and shown, so it may never carry err.Error()
// or a receiver's response body.
const (
	DeliveryErrTimeout = "timeout"
	DeliveryErrNetwork = "network"
	DeliveryErrBlocked = "blocked"
	DeliveryErrHTTP4xx = "http_4xx"
	DeliveryErrHTTP5xx = "http_5xx"
)

// ErrInvalidWebhook is returned for an endpoint the schema would refuse
// anyway (unknown purpose, empty URL or secret), so callers get a message
// instead of a constraint name.
var ErrInvalidWebhook = errors.New("invalid webhook endpoint")

// WebhookEndpoint is one outbound webhook target of a project.
type WebhookEndpoint struct {
	ID        int64  `json:"id"`
	ProjectID int64  `json:"project_id"`
	Purpose   string `json:"purpose"`
	URL       string `json:"url"`
	// Secret and PrevSecret are the plain `whsec_...` values, decrypted on
	// read for signing. They never leave the process: json:"-".
	Secret     string `json:"-"`
	PrevSecret string `json:"-"`
	// PrevExpiresAt is when PrevSecret stops being signed with; both are
	// empty/nil when no rotation is in its window. The signer compares it with
	// the clock; the claim and the janitor clear both once the window is over.
	PrevExpiresAt *time.Time `json:"prev_expires_at"`
	// DisabledAt is set when the receiver answered 410 Gone.
	DisabledAt *time.Time `json:"disabled_at"`
	CreatedBy  *int64     `json:"created_by"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

const webhookEndpointCols = `id, project_id, purpose, url, secret_enc, key_version, prev_secret_enc,
	prev_expires_at, disabled_at, created_by, created_at, updated_at`

func (s *Store) scanWebhookEndpoint(row pgx.Row) (*WebhookEndpoint, error) {
	var (
		e       WebhookEndpoint
		secret  []byte
		prev    []byte
		version int16
	)
	if err := row.Scan(&e.ID, &e.ProjectID, &e.Purpose, &e.URL, &secret, &version, &prev,
		&e.PrevExpiresAt, &e.DisabledAt, &e.CreatedBy, &e.CreatedAt, &e.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if version != keyVersionBound {
		return nil, fmt.Errorf("%w: webhook endpoint %d: unsupported key version %d", errSecretUnreadable, e.ID, version)
	}
	if err := s.openEndpointSecrets(&e, secret, prev); err != nil {
		return nil, err
	}
	return &e, nil
}

// errSecretUnreadable marks an endpoint whose current secret cannot be opened
// with this store's key. On its own it does not say why: a damaged row and a
// replica running with the wrong SECRET_KEY look the same here. The claim tells
// them apart with the instance canary (requireCanary) before it writes any state
// that cannot be undone.
var errSecretUnreadable = errors.New("webhook endpoint secret cannot be opened")

// openEndpointSecrets decrypts e's secrets from their blobs. The current
// secret must open. The previous one only matters during a rotation window, so
// a blob that does not open is dropped with a warning rather than taking the
// endpoint down with it.
func (s *Store) openEndpointSecrets(e *WebhookEndpoint, secret, prev []byte) error {
	aad := sealedAAD(webhookAADPrefix, e.ID)
	var err error
	if e.Secret, err = s.cipher.decrypt(secret, aad); err != nil {
		return fmt.Errorf("%w: webhook endpoint %d: %v", errSecretUnreadable, e.ID, err)
	}
	if e.PrevSecret, err = s.cipher.decrypt(prev, aad); err != nil {
		e.PrevSecret = ""
		s.log.Warn("previous webhook secret cannot be opened and is ignored", "endpoint", e.ID)
	}
	return nil
}

// CreateWebhookEndpoint stores a new endpoint for (ProjectID, Purpose) with
// the plain Secret sealed under the endpoint's own id. The row is inserted
// first so the id is known, then the secret is sealed; both in one
// transaction. A second endpoint for the same project and purpose fails with a
// unique violation (IsUniqueViolation).
func (s *Store) CreateWebhookEndpoint(ctx context.Context, e *WebhookEndpoint) (*WebhookEndpoint, error) {
	if e.Purpose != WebhookPurposeGuardrail && e.Purpose != WebhookPurposeAlerts {
		return nil, fmt.Errorf("%w: unknown purpose %q", ErrInvalidWebhook, e.Purpose)
	}
	if e.URL == "" || len(e.URL) > 2048 {
		return nil, fmt.Errorf("%w: url must be 1 to 2048 bytes", ErrInvalidWebhook)
	}
	if e.Secret == "" {
		return nil, fmt.Errorf("%w: secret is required", ErrInvalidWebhook)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful commit
	var id int64
	// The empty placeholder lives only until the UPDATE below, inside this
	// transaction; secret_enc is NOT NULL.
	if err := tx.QueryRow(ctx, `INSERT INTO webhook_endpoints
		(project_id, purpose, url, secret_enc, key_version, created_by)
		VALUES ($1, $2, $3, ''::bytea, $4, $5) RETURNING id`,
		e.ProjectID, e.Purpose, e.URL, keyVersionBound, e.CreatedBy).Scan(&id); err != nil {
		return nil, err
	}
	enc, err := s.cipher.encrypt(e.Secret, sealedAAD(webhookAADPrefix, id))
	if err != nil {
		return nil, err
	}
	out, err := s.scanWebhookEndpoint(tx.QueryRow(ctx,
		"UPDATE webhook_endpoints SET secret_enc = $1 WHERE id = $2 RETURNING "+webhookEndpointCols, enc, id))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

// GetWebhookEndpoint returns the endpoint of a project for a purpose, or
// ErrNotFound.
func (s *Store) GetWebhookEndpoint(ctx context.Context, projectID int64, purpose string) (*WebhookEndpoint, error) {
	return s.scanWebhookEndpoint(s.pool.QueryRow(ctx,
		"SELECT "+webhookEndpointCols+" FROM webhook_endpoints WHERE project_id = $1 AND purpose = $2", projectID, purpose))
}

// GetWebhookEndpointByID returns an endpoint by id, or ErrNotFound.
func (s *Store) GetWebhookEndpointByID(ctx context.Context, id int64) (*WebhookEndpoint, error) {
	return s.scanWebhookEndpoint(s.pool.QueryRow(ctx,
		"SELECT "+webhookEndpointCols+" FROM webhook_endpoints WHERE id = $1", id))
}

// UpdateWebhookEndpointURL points an endpoint at a new URL. A new URL is a new
// receiver, so a disabled endpoint (410 Gone) is enabled again. The secret is
// kept. ErrNotFound when the endpoint does not exist.
func (s *Store) UpdateWebhookEndpointURL(ctx context.Context, id int64, url string) (*WebhookEndpoint, error) {
	if url == "" || len(url) > 2048 {
		return nil, fmt.Errorf("%w: url must be 1 to 2048 bytes", ErrInvalidWebhook)
	}
	return s.scanWebhookEndpoint(s.pool.QueryRow(ctx, `UPDATE webhook_endpoints
		SET url = $2, disabled_at = NULL, updated_at = now() WHERE id = $1 RETURNING `+webhookEndpointCols, id, url))
}

// RotateWebhookSecret makes newSecret the endpoint's signing secret and keeps
// the old one in prev_secret_enc for keepPrev, during which the receiver can
// verify with either. Rotating again inside the window replaces the previous
// secret with the one just retired. One statement, so the swap is atomic.
func (s *Store) RotateWebhookSecret(ctx context.Context, id int64, newSecret string, keepPrev time.Duration) (*WebhookEndpoint, error) {
	if newSecret == "" {
		return nil, fmt.Errorf("%w: secret is required", ErrInvalidWebhook)
	}
	enc, err := s.cipher.encrypt(newSecret, sealedAAD(webhookAADPrefix, id))
	if err != nil {
		return nil, err
	}
	return s.scanWebhookEndpoint(s.pool.QueryRow(ctx, `UPDATE webhook_endpoints
		SET prev_secret_enc = secret_enc, prev_expires_at = now() + make_interval(secs => $3),
		    secret_enc = $2, key_version = $4, updated_at = now()
		WHERE id = $1 RETURNING `+webhookEndpointCols, id, enc, keepPrev.Seconds(), keyVersionBound))
}

// DeleteWebhookEndpoint removes an endpoint and, by cascade, its deliveries.
func (s *Store) DeleteWebhookEndpoint(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx, "DELETE FROM webhook_endpoints WHERE id = $1", id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// DisableWebhookEndpoint marks the endpoint gone (the receiver answered 410)
// and fails the deliveries still waiting for it, so nothing new is sent and
// nothing sits pending forever. Idempotent; ErrNotFound for an unknown id.
func (s *Store) DisableWebhookEndpoint(ctx context.Context, id int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful commit
	tag, err := tx.Exec(ctx, `UPDATE webhook_endpoints
		SET disabled_at = COALESCE(disabled_at, now()), updated_at = now() WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if _, err := tx.Exec(ctx, `UPDATE webhook_deliveries SET status = $2
		WHERE endpoint_id = $1 AND status = $3`, id, DeliveryFailed, DeliveryPending); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// WebhookDelivery is one row of the outbox. Payload is the alert body; a
// delivery never holds a prompt or a response, and never the receiver's reply.
type WebhookDelivery struct {
	ID            int64           `json:"id"`
	EndpointID    int64           `json:"endpoint_id"`
	EventType     string          `json:"event_type"`
	DedupeKey     string          `json:"dedupe_key"`
	Payload       json.RawMessage `json:"payload"`
	Status        string          `json:"status"`
	Attempts      int             `json:"attempts"`
	NextAttemptAt time.Time       `json:"next_attempt_at"`
	// LastStatus is the HTTP status of the last attempt, nil when it never
	// got one. LastErrorClass is one of the DeliveryErr* values.
	LastStatus     *int       `json:"last_status"`
	LastErrorClass *string    `json:"last_error_class"`
	LastDurationMS *int       `json:"last_duration_ms"`
	CreatedAt      time.Time  `json:"created_at"`
	DeliveredAt    *time.Time `json:"delivered_at"`
}

const webhookDeliveryCols = `id, endpoint_id, event_type, dedupe_key, payload, status, attempts, next_attempt_at,
	last_status, last_error_class, last_duration_ms, created_at, delivered_at`

func scanWebhookDelivery(row pgx.Row) (*WebhookDelivery, error) {
	var d WebhookDelivery
	if err := row.Scan(&d.ID, &d.EndpointID, &d.EventType, &d.DedupeKey, &d.Payload, &d.Status, &d.Attempts,
		&d.NextAttemptAt, &d.LastStatus, &d.LastErrorClass, &d.LastDurationMS, &d.CreatedAt, &d.DeliveredAt); err != nil {
		return nil, err
	}
	return &d, nil
}

// EnqueueWebhookDelivery adds a delivery for the endpoint unless one with the
// same dedupe key already exists for it (ON CONFLICT DO NOTHING), and reports
// whether it added one. An endpoint that is disabled or does not exist gets
// nothing. payload must be valid JSON.
func (s *Store) EnqueueWebhookDelivery(ctx context.Context, endpointID int64, eventType, dedupeKey string, payload []byte) (bool, error) {
	if eventType == "" || dedupeKey == "" {
		return false, fmt.Errorf("%w: event type and dedupe key are required", ErrInvalidWebhook)
	}
	if !json.Valid(payload) {
		return false, fmt.Errorf("%w: payload is not valid JSON", ErrInvalidWebhook)
	}
	tag, err := s.pool.Exec(ctx, `INSERT INTO webhook_deliveries (endpoint_id, event_type, dedupe_key, payload)
		SELECT id, $2::text, $3::text, $4::jsonb FROM webhook_endpoints WHERE id = $1 AND disabled_at IS NULL
		ON CONFLICT (endpoint_id, dedupe_key) DO NOTHING`, endpointID, eventType, dedupeKey, string(payload))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ClaimedWebhookDelivery is a delivery a worker now owns, with the endpoint it
// goes to (secrets decrypted).
type ClaimedWebhookDelivery struct {
	Delivery WebhookDelivery
	Endpoint *WebhookEndpoint
}

// ClaimWebhookDeliveries takes up to limit pending deliveries that are due and
// leases them for lease: next_attempt_at moves into the future and attempts
// counts the claim. FOR UPDATE SKIP LOCKED is what lets every replica (and
// every worker goroutine) poll at once without two of them taking the same
// row. A worker that dies mid-send leaves a row whose lease runs out and which
// is then claimed again; the attempt it burned stays counted.
//
// Deliveries of a disabled endpoint are not claimed.
//
// An endpoint whose own secret cannot be opened, while the instance key can
// (the canary opens), is a damaged row: the claimed deliveries of that endpoint
// are failed and left out, and the count per purpose comes back as dropped so
// the caller can meter them. When the canary does not open either, the key is
// wrong for this whole database (a rotation ran under a live replica, say); the
// claim then returns an error and rolls back, touching no row, so the
// deliveries are still there once the replica restarts with the right key.
func (s *Store) ClaimWebhookDeliveries(ctx context.Context, limit int, lease time.Duration) ([]ClaimedWebhookDelivery, map[string]int, error) {
	if limit <= 0 {
		return nil, nil, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful commit
	rows, err := tx.Query(ctx, `WITH due AS (
			SELECT d.id AS due_id FROM webhook_deliveries d
			  JOIN webhook_endpoints e ON e.id = d.endpoint_id
			 WHERE d.status = $3 AND d.next_attempt_at <= now() AND e.disabled_at IS NULL
			 ORDER BY d.next_attempt_at, d.id
			 LIMIT $1
			 FOR UPDATE OF d SKIP LOCKED)
		UPDATE webhook_deliveries d
		   SET attempts = d.attempts + 1, next_attempt_at = now() + make_interval(secs => $2)
		  FROM due WHERE d.id = due.due_id
		RETURNING `+webhookDeliveryCols, limit, lease.Seconds(), DeliveryPending)
	if err != nil {
		return nil, nil, err
	}
	var out []ClaimedWebhookDelivery
	for rows.Next() {
		d, err := scanWebhookDelivery(rows)
		if err != nil {
			rows.Close()
			return nil, nil, err
		}
		out = append(out, ClaimedWebhookDelivery{Delivery: *d})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	// The claimed ids of each endpoint: a damaged endpoint fails exactly these
	// rows, which this transaction already holds locked.
	byEndpoint := map[int64][]int64{}
	for _, c := range out {
		byEndpoint[c.Delivery.EndpointID] = append(byEndpoint[c.Delivery.EndpointID], c.Delivery.ID)
	}
	endpoints := map[int64]*WebhookEndpoint{} // nil value: damaged, dropped
	dropped := map[string]int{}
	ready := out[:0]
	for _, c := range out {
		id := c.Delivery.EndpointID
		ep, seen := endpoints[id]
		if !seen {
			var purpose string
			ep, purpose, err = s.loadClaimedEndpoint(ctx, tx, id, byEndpoint[id])
			if err != nil {
				return nil, nil, err
			}
			endpoints[id] = ep
			if ep == nil {
				dropped[purpose] += len(byEndpoint[id])
			}
		}
		if ep == nil {
			continue
		}
		c.Endpoint = ep
		ready = append(ready, c)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, err
	}
	return ready, dropped, nil
}

// loadClaimedEndpoint reads the endpoint of claimed deliveries inside the claim
// transaction.
//
// A secret that does not open is told apart by the canary. If the canary opens
// with this store's key, the key is right and this one row is damaged: the
// claimed deliveries (ids) are failed, the purpose is returned with a nil
// endpoint, and the claim goes on, so one bad row cannot hold back every other
// project's webhooks. If the canary does not open, the key itself is wrong for
// this database and an error comes back: the claim rolls back, nothing is
// failed, and nothing is lost for a replica that is later given the right key.
//
// An expired previous secret is cleared here: it is past its window and not
// worth keeping sealed.
func (s *Store) loadClaimedEndpoint(ctx context.Context, tx pgx.Tx, id int64, ids []int64) (*WebhookEndpoint, string, error) {
	ep, err := s.scanWebhookEndpoint(tx.QueryRow(ctx,
		"SELECT "+webhookEndpointCols+" FROM webhook_endpoints WHERE id = $1", id))
	switch {
	case errors.Is(err, errSecretUnreadable):
		if err := s.requireCanary(ctx, tx); err != nil {
			return nil, "", fmt.Errorf("webhook endpoint %d cannot be opened and %w", id, err)
		}
		var purpose string
		if err := tx.QueryRow(ctx, "SELECT purpose FROM webhook_endpoints WHERE id = $1", id).Scan(&purpose); err != nil {
			return nil, "", err
		}
		s.log.Warn("webhook endpoint secret cannot be opened; the claimed deliveries are failed",
			"endpoint", id, "deliveries", len(ids))
		if _, err := tx.Exec(ctx, `UPDATE webhook_deliveries SET status = $2
			WHERE id = ANY($1) AND status = $3`, ids, DeliveryFailed, DeliveryPending); err != nil {
			return nil, "", err
		}
		return nil, purpose, nil
	case err != nil:
		return nil, "", fmt.Errorf("load webhook endpoint %d: %w", id, err)
	}
	if ep.PrevExpiresAt != nil {
		tag, err := tx.Exec(ctx, `UPDATE webhook_endpoints SET prev_secret_enc = NULL, prev_expires_at = NULL
			WHERE id = $1 AND prev_expires_at <= now()`, id)
		if err != nil {
			return nil, "", err
		}
		if tag.RowsAffected() == 1 {
			ep.PrevSecret, ep.PrevExpiresAt = "", nil
		}
	}
	return ep, ep.Purpose, nil
}

// requireCanary checks that the instance canary opens with this store's key. A
// nil result means the key is the right one for the database; an error means
// it is not, or that this could not be established (no canary stored), and the
// caller must not write any state that cannot be undone.
func (s *Store) requireCanary(ctx context.Context, tx pgx.Tx) error {
	var stored []byte
	if err := tx.QueryRow(ctx, "SELECT value FROM instance_settings WHERE key = $1", settingSecretKeyCanary).Scan(&stored); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errors.New("the instance key cannot be verified (no canary stored)")
		}
		return err
	}
	if plain, err := s.cipher.decrypt(stored, canaryAAD()); err != nil || plain != canaryPlaintext {
		return fmt.Errorf("%w: the canary does not open with this replica's key", ErrSecretKeyMismatch)
	}
	return nil
}

// ClearExpiredWebhookSecrets drops every previous secret whose rotation window
// has ended and reports how many endpoints it touched. The window is 24 hours
// (ADR-033); keeping a retired secret sealed in the database any longer only
// keeps it recoverable for whoever holds the key.
func (s *Store) ClearExpiredWebhookSecrets(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE webhook_endpoints SET prev_secret_enc = NULL, prev_expires_at = NULL
		WHERE prev_expires_at <= now()`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ReleaseWebhookDeliveries hands claimed deliveries back, due at once, with the
// attempt given back: a worker that is shutting down calls it for the claims it
// never started, so they are not lost for a lease nor charged an attempt.
func (s *Store) ReleaseWebhookDeliveries(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx, `UPDATE webhook_deliveries
		SET attempts = GREATEST(attempts - 1, 0), next_attempt_at = now()
		WHERE id = ANY($1) AND status = $2`, ids, DeliveryPending)
	return err
}

// WebhookDeliveryResult is the outcome of one attempt, written by
// FinishWebhookDelivery.
type WebhookDeliveryResult struct {
	// Status is the delivery's new state: DeliveryPending to try again after
	// RetryIn, DeliveryDelivered or DeliveryFailed.
	Status  string
	RetryIn time.Duration
	// HTTPStatus is the response status, nil when the attempt got none.
	HTTPStatus *int
	// ErrorClass is one of the DeliveryErr* values, empty on success.
	ErrorClass string
	DurationMS int
}

// FinishWebhookDelivery records an attempt. It only touches a row that is
// still pending, so a delivery already failed by DisableWebhookEndpoint is
// left alone. ErrNotFound when the row is gone or no longer pending.
func (s *Store) FinishWebhookDelivery(ctx context.Context, id int64, r WebhookDeliveryResult) error {
	switch r.Status {
	case DeliveryPending, DeliveryDelivered, DeliveryFailed:
	default:
		return fmt.Errorf("store: unknown delivery status %q", r.Status)
	}
	var class any
	if r.ErrorClass != "" {
		class = r.ErrorClass
	}
	var httpStatus any
	if r.HTTPStatus != nil {
		httpStatus = *r.HTTPStatus
	}
	tag, err := s.pool.Exec(ctx, `UPDATE webhook_deliveries SET status = $2,
			next_attempt_at = CASE WHEN $2 = 'pending' THEN now() + make_interval(secs => $3) ELSE next_attempt_at END,
			last_status = $4, last_error_class = $5, last_duration_ms = $6,
			delivered_at = CASE WHEN $2 = 'delivered' THEN now() ELSE NULL END
		WHERE id = $1 AND status = 'pending'`,
		id, r.Status, r.RetryIn.Seconds(), httpStatus, class, r.DurationMS)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListWebhookDeliveries returns the newest deliveries of an endpoint first.
func (s *Store) ListWebhookDeliveries(ctx context.Context, endpointID int64, limit int) ([]WebhookDelivery, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, "SELECT "+webhookDeliveryCols+
		" FROM webhook_deliveries WHERE endpoint_id = $1 ORDER BY id DESC LIMIT $2", endpointID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WebhookDelivery
	for rows.Next() {
		d, err := scanWebhookDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}
