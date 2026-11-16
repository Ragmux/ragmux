package store_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/internal/testdb"
)

const (
	secretA = "whsec_QUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUE="
	secretB = "whsec_QkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkI="
)

// webhookFixture is a store with one project and two endpoints (one per
// purpose).
type webhookFixture struct {
	s       *store.Store
	project *store.Project
	alerts  *store.WebhookEndpoint
	guard   *store.WebhookEndpoint
}

func newWebhookFixture(t *testing.T) *webhookFixture {
	t.Helper()
	ctx := context.Background()
	s := testdb.Open(t)
	conn, err := s.CreateConnection(ctx, &store.ModelConnection{Name: "c", ProviderType: "openai", APIKey: "k", ModelName: "m"})
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := s.CreateProject(ctx, &store.Project{Name: "p", ModelConnectionID: conn.ID})
	if err != nil {
		t.Fatal(err)
	}
	alerts, err := s.CreateWebhookEndpoint(ctx, &store.WebhookEndpoint{ProjectID: p.ID, Purpose: store.WebhookPurposeAlerts, URL: "https://hooks.example/alerts", Secret: secretA})
	if err != nil {
		t.Fatal(err)
	}
	guard, err := s.CreateWebhookEndpoint(ctx, &store.WebhookEndpoint{ProjectID: p.ID, Purpose: store.WebhookPurposeGuardrail, URL: "https://hooks.example/guard", Secret: secretB})
	if err != nil {
		t.Fatal(err)
	}
	return &webhookFixture{s: s, project: p, alerts: alerts, guard: guard}
}

func (f *webhookFixture) enqueue(t *testing.T, ep *store.WebhookEndpoint, key string) {
	t.Helper()
	ok, err := f.s.EnqueueWebhookDelivery(context.Background(), ep.ID, "limit.warning", key, []byte(`{"limit":"rpm"}`))
	if err != nil || !ok {
		t.Fatalf("enqueue %q: ok=%v err=%v", key, ok, err)
	}
}

func (f *webhookFixture) count(t *testing.T, where string, args ...any) int {
	t.Helper()
	var n int
	if err := f.s.DB().QueryRow(context.Background(), "SELECT COUNT(*) FROM webhook_deliveries "+where, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestWebhookSecretIsSealedAndBoundToItsRow(t *testing.T) {
	ctx := context.Background()
	f := newWebhookFixture(t)

	var raw []byte
	var version int16
	if err := f.s.DB().QueryRow(ctx, "SELECT secret_enc, key_version FROM webhook_endpoints WHERE id = $1", f.alerts.ID).Scan(&raw, &version); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(secretA)) || bytes.Contains(raw, []byte("QUFBQUFB")) {
		t.Error("the secret is stored in plain text")
	}
	if len(raw) <= len(secretA) || version != 1 {
		t.Errorf("secret_enc %d bytes at key_version %d; want sealed (nonce+tag longer than the secret) at version 1", len(raw), version)
	}
	got, err := f.s.GetWebhookEndpoint(ctx, f.project.ID, store.WebhookPurposeAlerts)
	if err != nil || got.Secret != secretA || got.PrevSecret != "" {
		t.Fatalf("read back: %+v, %v", got, err)
	}
	if byID, err := f.s.GetWebhookEndpointByID(ctx, f.guard.ID); err != nil || byID.Secret != secretB {
		t.Fatalf("by id: %+v, %v", byID, err)
	}

	// A blob copied onto another row does not open there (AAD = row id).
	if _, err := f.s.DB().Exec(ctx, "UPDATE webhook_endpoints SET secret_enc = $1 WHERE id = $2", raw, f.guard.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.GetWebhookEndpointByID(ctx, f.guard.ID); err == nil {
		t.Error("a secret sealed for another row was opened")
	}
	if _, err := f.s.GetWebhookEndpointByID(ctx, f.alerts.ID); err != nil {
		t.Errorf("the untouched row stopped opening: %v", err)
	}
}

func TestWebhookEndpointIsUniquePerProjectAndPurposeAndValidated(t *testing.T) {
	ctx := context.Background()
	f := newWebhookFixture(t)
	_, err := f.s.CreateWebhookEndpoint(ctx, &store.WebhookEndpoint{ProjectID: f.project.ID, Purpose: store.WebhookPurposeAlerts, URL: "https://x.example", Secret: secretA})
	if !store.IsUniqueViolation(err) {
		t.Errorf("second alerts endpoint: %v, want a unique violation", err)
	}
	for name, e := range map[string]*store.WebhookEndpoint{
		"purpose": {ProjectID: f.project.ID, Purpose: "metrics", URL: "https://x.example", Secret: secretA},
		"url":     {ProjectID: f.project.ID, Purpose: "alerts", URL: "", Secret: secretA},
		"secret":  {ProjectID: f.project.ID, Purpose: "alerts", URL: "https://x.example"},
	} {
		if _, err := f.s.CreateWebhookEndpoint(ctx, e); !errors.Is(err, store.ErrInvalidWebhook) {
			t.Errorf("%s: %v, want ErrInvalidWebhook", name, err)
		}
	}
	if _, err := f.s.GetWebhookEndpoint(ctx, f.project.ID+999, store.WebhookPurposeAlerts); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown project: %v, want ErrNotFound", err)
	}
	// The project owns its endpoints.
	if err := f.s.DeleteProject(ctx, f.project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.GetWebhookEndpointByID(ctx, f.alerts.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("endpoint outlived its project: %v", err)
	}
}

func TestRotateWebhookSecretKeepsThePreviousOneForTheWindow(t *testing.T) {
	ctx := context.Background()
	f := newWebhookFixture(t)
	const third = "whsec_Q0NDQ0NDQ0NDQ0NDQ0NDQ0NDQ0NDQ0NDQ0NDQ0NDQ0M="

	before := time.Now()
	rot, err := f.s.RotateWebhookSecret(ctx, f.alerts.ID, secretB, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if rot.Secret != secretB || rot.PrevSecret != secretA {
		t.Fatalf("after rotation: secret/prev = %q/%q", rot.Secret, rot.PrevSecret)
	}
	if rot.PrevExpiresAt == nil || rot.PrevExpiresAt.Before(before.Add(23*time.Hour)) || rot.PrevExpiresAt.After(time.Now().Add(25*time.Hour)) {
		t.Errorf("prev_expires_at = %v, want about 24 h from now", rot.PrevExpiresAt)
	}
	var prevRaw []byte
	if err := f.s.DB().QueryRow(ctx, "SELECT prev_secret_enc FROM webhook_endpoints WHERE id = $1", f.alerts.ID).Scan(&prevRaw); err != nil {
		t.Fatal(err)
	}
	if len(prevRaw) == 0 || bytes.Contains(prevRaw, []byte("QUFBQUFB")) {
		t.Error("previous secret missing or stored in plain text")
	}
	// A second rotation retires the then-current secret; the older one is gone.
	rot, err = f.s.RotateWebhookSecret(ctx, f.alerts.ID, third, 24*time.Hour)
	if err != nil || rot.Secret != third || rot.PrevSecret != secretB {
		t.Fatalf("second rotation: %+v, %v", rot, err)
	}
	if _, err := f.s.RotateWebhookSecret(ctx, f.alerts.ID+999, secretA, time.Hour); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown endpoint: %v, want ErrNotFound", err)
	}
	if _, err := f.s.RotateWebhookSecret(ctx, f.alerts.ID, "", time.Hour); !errors.Is(err, store.ErrInvalidWebhook) {
		t.Errorf("empty secret: %v, want ErrInvalidWebhook", err)
	}
}

func TestEnqueueWebhookDeliveryDeduplicates(t *testing.T) {
	ctx := context.Background()
	f := newWebhookFixture(t)

	f.enqueue(t, f.alerts, "rpm:80:2026-11-04T12")
	ok, err := f.s.EnqueueWebhookDelivery(ctx, f.alerts.ID, "limit.warning", "rpm:80:2026-11-04T12", []byte(`{"limit":"rpm","again":true}`))
	if err != nil || ok {
		t.Errorf("same dedupe key: ok=%v err=%v, want no second row", ok, err)
	}
	f.enqueue(t, f.alerts, "rpm:80:2026-11-04T13")
	// The key is per endpoint: the other endpoint takes the same key.
	f.enqueue(t, f.guard, "rpm:80:2026-11-04T12")
	if n := f.count(t, ""); n != 3 {
		t.Errorf("%d rows, want 3", n)
	}
	list, err := f.s.ListWebhookDeliveries(ctx, f.alerts.ID, 10)
	if err != nil || len(list) != 2 {
		t.Fatalf("list: %d, %v", len(list), err)
	}
	var payload map[string]any
	if err := json.Unmarshal(list[1].Payload, &payload); err != nil || payload["limit"] != "rpm" || len(payload) != 1 {
		t.Errorf("payload = %s (%v), want the first body, not the duplicate's", list[1].Payload, err)
	}
	if list[1].Status != store.DeliveryPending || list[1].Attempts != 0 {
		t.Errorf("first delivery = %+v", list[1])
	}

	if _, err := f.s.EnqueueWebhookDelivery(ctx, f.alerts.ID, "limit.warning", "k", []byte(`not json`)); !errors.Is(err, store.ErrInvalidWebhook) {
		t.Errorf("invalid payload: %v, want ErrInvalidWebhook", err)
	}
	if _, err := f.s.EnqueueWebhookDelivery(ctx, f.alerts.ID, "", "k", []byte(`{}`)); !errors.Is(err, store.ErrInvalidWebhook) {
		t.Errorf("empty event type: %v, want ErrInvalidWebhook", err)
	}

	// A disabled endpoint, or one that does not exist, gets nothing new.
	if err := f.s.DisableWebhookEndpoint(ctx, f.guard.ID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{f.guard.ID, f.guard.ID + 999} {
		if ok, err := f.s.EnqueueWebhookDelivery(ctx, id, "limit.warning", "fresh", []byte(`{}`)); err != nil || ok {
			t.Errorf("endpoint %d: ok=%v err=%v, want nothing enqueued", id, ok, err)
		}
	}
}

func TestClaimWebhookDeliveriesLeasesAndCountsTheAttempt(t *testing.T) {
	ctx := context.Background()
	f := newWebhookFixture(t)
	f.enqueue(t, f.alerts, "a")
	f.enqueue(t, f.alerts, "b")
	// "b" is not due yet.
	if _, err := f.s.DB().Exec(ctx, "UPDATE webhook_deliveries SET next_attempt_at = now() + interval '1 hour' WHERE dedupe_key = 'b'"); err != nil {
		t.Fatal(err)
	}

	got, _, err := f.s.ClaimWebhookDeliveries(ctx, 10, 2*time.Minute)
	if err != nil || len(got) != 1 {
		t.Fatalf("claim: %d, %v", len(got), err)
	}
	c := got[0]
	if c.Delivery.DedupeKey != "a" || c.Delivery.Attempts != 1 || c.Endpoint == nil || c.Endpoint.Secret != secretA || c.Endpoint.URL != "https://hooks.example/alerts" {
		t.Errorf("claimed = %+v endpoint %+v", c.Delivery, c.Endpoint)
	}
	if !c.Delivery.NextAttemptAt.After(time.Now().Add(time.Minute)) {
		t.Errorf("lease ends %v, want about 2 min ahead", c.Delivery.NextAttemptAt)
	}
	// Leased: a second poll finds nothing; limit <= 0 claims nothing.
	if again, _, err := f.s.ClaimWebhookDeliveries(ctx, 10, time.Minute); err != nil || len(again) != 0 {
		t.Errorf("second claim: %d, %v", len(again), err)
	}
	if none, _, err := f.s.ClaimWebhookDeliveries(ctx, 0, time.Minute); err != nil || len(none) != 0 {
		t.Errorf("limit 0: %d, %v", len(none), err)
	}
	// A lease that ran out puts the row back in the queue (crashed worker).
	if _, err := f.s.DB().Exec(ctx, "UPDATE webhook_deliveries SET next_attempt_at = now() - interval '1 second' WHERE dedupe_key = 'a'"); err != nil {
		t.Fatal(err)
	}
	again, _, err := f.s.ClaimWebhookDeliveries(ctx, 10, time.Minute)
	if err != nil || len(again) != 1 || again[0].Delivery.Attempts != 2 {
		t.Errorf("after the lease expired: %d, %v (attempts should be 2)", len(again), err)
	}
}

// Two workers (or replicas) must never take the same row.
func TestClaimWebhookDeliveriesSkipsLockedRowsAndNeverDoubleClaims(t *testing.T) {
	ctx := context.Background()
	f := newWebhookFixture(t)
	for _, k := range []string{"a", "b", "c"} {
		f.enqueue(t, f.alerts, k)
	}

	// A row another transaction holds locked is skipped, not waited for.
	tx, err := f.s.DB().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var lockedID int64
	if err := tx.QueryRow(ctx, "SELECT id FROM webhook_deliveries WHERE dedupe_key = 'a' FOR UPDATE").Scan(&lockedID); err != nil {
		t.Fatal(err)
	}
	claimed, _, err := f.s.ClaimWebhookDeliveries(ctx, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, c := range claimed {
		keys = append(keys, c.Delivery.DedupeKey)
	}
	sort.Strings(keys)
	if len(keys) != 2 || keys[0] != "b" || keys[1] != "c" {
		t.Errorf("claimed %v while 'a' was locked, want [b c]", keys)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if rest, _, err := f.s.ClaimWebhookDeliveries(ctx, 10, time.Minute); err != nil || len(rest) != 1 || rest[0].Delivery.DedupeKey != "a" {
		t.Errorf("after the lock was released: %d, %v; want 'a'", len(rest), err)
	}

	// Concurrent pollers split a backlog without overlap.
	for i := 0; i < 40; i++ {
		f.enqueue(t, f.guard, "k"+string(rune('A'+i/26))+string(rune('a'+i%26)))
	}
	var (
		mu   sync.Mutex
		seen = map[int64]int{}
		wg   sync.WaitGroup
	)
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				got, _, err := f.s.ClaimWebhookDeliveries(ctx, 7, time.Minute)
				if err != nil {
					t.Error(err)
					return
				}
				if len(got) == 0 {
					return
				}
				mu.Lock()
				for _, c := range got {
					seen[c.Delivery.ID]++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(seen) != 40 {
		t.Errorf("%d distinct deliveries claimed, want 40", len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("delivery %d claimed %d times", id, n)
		}
	}
}

func TestFinishWebhookDelivery(t *testing.T) {
	ctx := context.Background()
	f := newWebhookFixture(t)
	f.enqueue(t, f.alerts, "a")
	got, _, err := f.s.ClaimWebhookDeliveries(ctx, 1, time.Minute)
	if err != nil || len(got) != 1 {
		t.Fatalf("claim: %d, %v", len(got), err)
	}
	id := got[0].Delivery.ID
	status := 503

	// retry
	if err := f.s.FinishWebhookDelivery(ctx, id, store.WebhookDeliveryResult{
		Status: store.DeliveryPending, RetryIn: 5 * time.Second, HTTPStatus: &status, ErrorClass: store.DeliveryErrHTTP5xx, DurationMS: 120,
	}); err != nil {
		t.Fatal(err)
	}
	d := f.mustDelivery(t, f.alerts.ID)
	if d.Status != store.DeliveryPending || d.LastStatus == nil || *d.LastStatus != 503 || d.LastErrorClass == nil || *d.LastErrorClass != "http_5xx" ||
		d.LastDurationMS == nil || *d.LastDurationMS != 120 || d.DeliveredAt != nil {
		t.Errorf("after a retry: %+v", d)
	}
	if !d.NextAttemptAt.After(time.Now().Add(2*time.Second)) || d.NextAttemptAt.After(time.Now().Add(10*time.Second)) {
		t.Errorf("next attempt %v, want about 5 s ahead", d.NextAttemptAt)
	}

	// delivered: clears the class, stamps delivered_at
	ok := 200
	if err := f.s.FinishWebhookDelivery(ctx, id, store.WebhookDeliveryResult{Status: store.DeliveryDelivered, HTTPStatus: &ok, DurationMS: 30}); err != nil {
		t.Fatal(err)
	}
	d = f.mustDelivery(t, f.alerts.ID)
	if d.Status != store.DeliveryDelivered || d.LastErrorClass != nil || d.DeliveredAt == nil || d.LastStatus == nil || *d.LastStatus != 200 {
		t.Errorf("after delivery: %+v", d)
	}
	// A settled row is not touched again.
	if err := f.s.FinishWebhookDelivery(ctx, id, store.WebhookDeliveryResult{Status: store.DeliveryFailed}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("finishing a settled row: %v, want ErrNotFound", err)
	}
	if err := f.s.FinishWebhookDelivery(ctx, id, store.WebhookDeliveryResult{Status: "weird"}); err == nil {
		t.Error("unknown status accepted")
	}

	// The error class is a closed set, enforced by the schema as well.
	f.enqueue(t, f.alerts, "b")
	var id2 int64
	if err := f.s.DB().QueryRow(ctx, "SELECT id FROM webhook_deliveries WHERE dedupe_key = 'b'").Scan(&id2); err != nil {
		t.Fatal(err)
	}
	if err := f.s.FinishWebhookDelivery(ctx, id2, store.WebhookDeliveryResult{Status: store.DeliveryFailed, ErrorClass: "Post https://hooks.example: boom"}); err == nil {
		t.Error("a free-text error class was stored")
	}
}

func (f *webhookFixture) mustDelivery(t *testing.T, endpointID int64) store.WebhookDelivery {
	t.Helper()
	list, err := f.s.ListWebhookDeliveries(context.Background(), endpointID, 1)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %d, %v", len(list), err)
	}
	return list[0]
}

func TestDisableWebhookEndpointFailsPendingDeliveriesAndStopsClaims(t *testing.T) {
	ctx := context.Background()
	f := newWebhookFixture(t)
	f.enqueue(t, f.alerts, "a")
	f.enqueue(t, f.alerts, "b")
	f.enqueue(t, f.guard, "g")

	if err := f.s.DisableWebhookEndpoint(ctx, f.alerts.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.s.DisableWebhookEndpoint(ctx, f.alerts.ID); err != nil {
		t.Errorf("disabling twice: %v", err)
	}
	if err := f.s.DisableWebhookEndpoint(ctx, f.alerts.ID+999); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown endpoint: %v, want ErrNotFound", err)
	}
	if n := f.count(t, "WHERE endpoint_id = $1 AND status = 'failed'", f.alerts.ID); n != 2 {
		t.Errorf("%d failed deliveries, want 2", n)
	}
	if n := f.count(t, "WHERE endpoint_id = $1 AND status = 'pending'", f.guard.ID); n != 1 {
		t.Errorf("the other endpoint's delivery was touched (%d pending)", n)
	}
	got, _, err := f.s.ClaimWebhookDeliveries(ctx, 10, time.Minute)
	if err != nil || len(got) != 1 || got[0].Delivery.DedupeKey != "g" {
		t.Errorf("claim after disable: %d, %v; want only the other endpoint's delivery", len(got), err)
	}
	if ep, err := f.s.GetWebhookEndpointByID(ctx, f.alerts.ID); err != nil || ep.DisabledAt == nil {
		t.Errorf("disabled_at not set: %+v, %v", ep, err)
	}
	// A new URL is a new receiver: the endpoint is enabled again.
	ep, err := f.s.UpdateWebhookEndpointURL(ctx, f.alerts.ID, "https://new.example/hook")
	if err != nil || ep.DisabledAt != nil || ep.URL != "https://new.example/hook" || ep.Secret != secretA {
		t.Errorf("after URL update: %+v, %v", ep, err)
	}
}

func TestDeletingAnEndpointRemovesItsDeliveries(t *testing.T) {
	ctx := context.Background()
	f := newWebhookFixture(t)
	f.enqueue(t, f.alerts, "a")
	if err := f.s.DeleteWebhookEndpoint(ctx, f.alerts.ID); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, ""); n != 0 {
		t.Errorf("%d deliveries left", n)
	}
	if err := f.s.DeleteWebhookEndpoint(ctx, f.alerts.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("deleting twice: %v, want ErrNotFound", err)
	}
}

func TestRetentionCoversWebhookDeliveries(t *testing.T) {
	ctx := context.Background()
	f := newWebhookFixture(t)
	for _, k := range []string{"old1", "old2", "fresh"} {
		f.enqueue(t, f.alerts, k)
	}
	if _, err := f.s.DB().Exec(ctx, "UPDATE webhook_deliveries SET created_at = now() - interval '100 days' WHERE dedupe_key LIKE 'old%'"); err != nil {
		t.Fatal(err)
	}
	cutoff := time.Now().AddDate(0, 0, -90)
	n, err := f.s.DeleteExpiredBatch(ctx, store.RetainWebhookDeliveries, cutoff, 10)
	if err != nil || n != 2 {
		t.Fatalf("DeleteExpiredBatch = (%d, %v), want (2, nil)", n, err)
	}
	if left := f.count(t, ""); left != 1 {
		t.Errorf("%d rows left, want the fresh one", left)
	}
	if got := store.RetainWebhookDeliveries.String(); got != "webhook_deliveries" {
		t.Errorf("String() = %q", got)
	}
}

// One endpoint whose secret cannot be opened must not hold back the others:
// the claimed deliveries of that endpoint are failed (and reported as dropped),
// the rest of the batch is claimed, and a later poll does not meet the same
// row at the head of the queue.
func TestClaimIsolatesAnEndpointWhoseSecretCannotBeOpened(t *testing.T) {
	ctx := context.Background()
	f := newWebhookFixture(t)
	// Oldest first, so the damaged endpoint's delivery heads the queue.
	f.enqueue(t, f.alerts, "broken-1")
	f.enqueue(t, f.alerts, "broken-2")
	f.enqueue(t, f.guard, "healthy-1")
	f.enqueue(t, f.guard, "healthy-2")
	if _, err := f.s.DB().Exec(ctx, "UPDATE webhook_endpoints SET secret_enc = $1 WHERE id = $2", bytes.Repeat([]byte{7}, 64), f.alerts.ID); err != nil {
		t.Fatal(err)
	}

	got, dropped, err := f.s.ClaimWebhookDeliveries(ctx, 10, time.Minute)
	if err != nil {
		t.Fatalf("a damaged endpoint failed the whole claim: %v", err)
	}
	var keys []string
	for _, c := range got {
		keys = append(keys, c.Delivery.DedupeKey)
		if c.Endpoint == nil || c.Endpoint.ID != f.guard.ID {
			t.Errorf("claimed %q with endpoint %+v", c.Delivery.DedupeKey, c.Endpoint)
		}
	}
	sort.Strings(keys)
	if len(keys) != 2 || keys[0] != "healthy-1" || keys[1] != "healthy-2" {
		t.Errorf("claimed %v, want both healthy deliveries", keys)
	}
	if dropped[store.WebhookPurposeAlerts] != 2 || len(dropped) != 1 {
		t.Errorf("dropped = %v, want 2 for alerts", dropped)
	}
	if n := f.count(t, "WHERE endpoint_id = $1 AND status = 'failed'", f.alerts.ID); n != 2 {
		t.Errorf("%d of the damaged endpoint's deliveries failed, want 2", n)
	}
	// A delivery that arrives later is failed by the claim that picks it up,
	// not queued behind the damaged row.
	f.enqueue(t, f.alerts, "broken-3")
	again, dropped, err := f.s.ClaimWebhookDeliveries(ctx, 10, time.Minute)
	if err != nil || len(again) != 0 || dropped[store.WebhookPurposeAlerts] != 1 {
		t.Errorf("second claim: %d, dropped %v, %v", len(again), dropped, err)
	}
	if n := f.count(t, "WHERE endpoint_id = $1 AND status = 'pending'", f.alerts.ID); n != 0 {
		t.Errorf("%d deliveries of the damaged endpoint still pending", n)
	}
}

// A replica that runs with the wrong SECRET_KEY cannot open any endpoint. That
// is a key problem, not damaged rows: nothing may be failed, so the deliveries
// are there once the replica has the right key.
func TestClaimWithTheWrongKeyFailsNothing(t *testing.T) {
	ctx := context.Background()
	cfg := testdb.Config(t)
	right := testdb.OpenWith(t, cfg)
	conn, err := right.CreateConnection(ctx, &store.ModelConnection{Name: "c", ProviderType: "openai", APIKey: "k", ModelName: "m"})
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := right.CreateProject(ctx, &store.Project{Name: "p", ModelConnectionID: conn.ID})
	if err != nil {
		t.Fatal(err)
	}
	for _, purpose := range []string{store.WebhookPurposeAlerts, store.WebhookPurposeGuardrail} {
		ep, err := right.CreateWebhookEndpoint(ctx, &store.WebhookEndpoint{ProjectID: p.ID, Purpose: purpose, URL: "https://hooks.example/" + purpose, Secret: secretA})
		if err != nil {
			t.Fatal(err)
		}
		for _, k := range []string{"a", "b", "c"} {
			if ok, err := right.EnqueueWebhookDelivery(ctx, ep.ID, "limit.warning", k, []byte(`{}`)); err != nil || !ok {
				t.Fatalf("enqueue: %v %v", ok, err)
			}
		}
	}
	// The replica that missed a key rotation: the database now holds secrets
	// and a canary sealed with another key. Simulate it by rotating under the
	// running (old-key) store.
	if _, err := right.ReencryptSealed(ctx, rotatedKey); err != nil {
		t.Fatal(err)
	}

	got, dropped, err := right.ClaimWebhookDeliveries(ctx, 20, time.Minute)
	if err == nil {
		t.Fatalf("a claim under the wrong key succeeded: %d claimed, dropped %v", len(got), dropped)
	}
	if !errors.Is(err, store.ErrSecretKeyMismatch) {
		t.Errorf("error = %v, want it to wrap ErrSecretKeyMismatch", err)
	}
	var n, failed, attempts int
	if err := right.DB().QueryRow(ctx, `SELECT COUNT(*), COUNT(*) FILTER (WHERE status = 'failed'), COALESCE(SUM(attempts), 0)
		FROM webhook_deliveries`).Scan(&n, &failed, &attempts); err != nil {
		t.Fatal(err)
	}
	if n != 6 || failed != 0 || attempts != 0 {
		t.Errorf("after the wrong-key claim: %d rows, %d failed, %d attempts; want 6, 0, 0 (nothing touched)", n, failed, attempts)
	}

	// The replica restarted with the right (new) key delivers them.
	cfg.SecretKeyHex = rotatedKey
	fixed := testdb.OpenWith(t, cfg)
	got, dropped, err = fixed.ClaimWebhookDeliveries(ctx, 20, time.Minute)
	if err != nil || len(got) != 6 || len(dropped) != 0 {
		t.Fatalf("with the right key: %d claimed, dropped %v, %v; want 6 and none dropped", len(got), dropped, err)
	}
	for _, c := range got {
		if c.Endpoint.Secret != secretA {
			t.Errorf("secret = %q", c.Endpoint.Secret)
		}
	}
}

// The canary is what tells a damaged row from a wrong key. When it cannot vouch
// for the key -- it is gone, or it does not open -- an endpoint that does not
// open must fail the claim and leave every row alone, never be failed for good.
func TestClaimWithADamagedEndpointNeedsAVerifiableKey(t *testing.T) {
	for name, breakCanary := range map[string]string{
		"canary row missing": "DELETE FROM instance_settings WHERE key = 'secret_key_canary'",
		"canary corrupt":     "UPDATE instance_settings SET value = '\\x0707070707070707070707070707070707070707070707070707070707070707'::bytea WHERE key = 'secret_key_canary'",
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			f := newWebhookFixture(t)
			f.enqueue(t, f.alerts, "broken")
			f.enqueue(t, f.guard, "healthy")
			if _, err := f.s.DB().Exec(ctx, "UPDATE webhook_endpoints SET secret_enc = $1 WHERE id = $2", bytes.Repeat([]byte{7}, 64), f.alerts.ID); err != nil {
				t.Fatal(err)
			}
			if tag, err := f.s.DB().Exec(ctx, breakCanary); err != nil || tag.RowsAffected() != 1 {
				t.Fatalf("breaking the canary: %v (rows %d)", err, tag.RowsAffected())
			}

			got, dropped, err := f.s.ClaimWebhookDeliveries(ctx, 10, time.Minute)
			if err == nil {
				t.Fatalf("claim succeeded without a verifiable key: %d claimed, dropped %v", len(got), dropped)
			}
			var failed, attempts int
			if err := f.s.DB().QueryRow(ctx, `SELECT COUNT(*) FILTER (WHERE status = 'failed'), COALESCE(SUM(attempts), 0)
				FROM webhook_deliveries`).Scan(&failed, &attempts); err != nil {
				t.Fatal(err)
			}
			if failed != 0 || attempts != 0 {
				t.Errorf("%d failed and %d attempts after the refused claim; want nothing touched", failed, attempts)
			}
		})
	}
}

// An unreadable previous secret is not worth losing the endpoint over, and a
// previous secret past its window is dropped.
func TestClaimToleratesAnUnreadablePreviousSecretAndClearsExpiredOnes(t *testing.T) {
	ctx := context.Background()
	f := newWebhookFixture(t)
	if _, err := f.s.RotateWebhookSecret(ctx, f.alerts.ID, secretB, 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.DB().Exec(ctx, "UPDATE webhook_endpoints SET prev_secret_enc = $1 WHERE id = $2", bytes.Repeat([]byte{7}, 64), f.alerts.ID); err != nil {
		t.Fatal(err)
	}
	f.enqueue(t, f.alerts, "a")
	got, _, err := f.s.ClaimWebhookDeliveries(ctx, 10, time.Minute)
	if err != nil || len(got) != 1 || got[0].Endpoint.Secret != secretB || got[0].Endpoint.PrevSecret != "" {
		t.Fatalf("claim with a damaged previous secret: %d, %v", len(got), err)
	}

	// Window over: the claim drops the retired secret.
	if _, err := f.s.DB().Exec(ctx, "UPDATE webhook_endpoints SET prev_expires_at = now() - interval '1 minute' WHERE id = $1", f.alerts.ID); err != nil {
		t.Fatal(err)
	}
	f.enqueue(t, f.alerts, "b")
	got, _, err = f.s.ClaimWebhookDeliveries(ctx, 10, time.Minute)
	if err != nil || len(got) != 1 || got[0].Endpoint.PrevSecret != "" || got[0].Endpoint.PrevExpiresAt != nil {
		t.Fatalf("claim after the window: %d, %v", len(got), err)
	}
	var prevRaw []byte
	var exp *time.Time
	if err := f.s.DB().QueryRow(ctx, "SELECT prev_secret_enc, prev_expires_at FROM webhook_endpoints WHERE id = $1", f.alerts.ID).Scan(&prevRaw, &exp); err != nil {
		t.Fatal(err)
	}
	if prevRaw != nil || exp != nil {
		t.Errorf("retired secret still stored: %d bytes, expires %v", len(prevRaw), exp)
	}
}

func TestClearExpiredWebhookSecrets(t *testing.T) {
	ctx := context.Background()
	f := newWebhookFixture(t)
	for _, id := range []int64{f.alerts.ID, f.guard.ID} {
		if _, err := f.s.RotateWebhookSecret(ctx, id, secretB, 24*time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.s.DB().Exec(ctx, "UPDATE webhook_endpoints SET prev_expires_at = now() - interval '1 second' WHERE id = $1", f.alerts.ID); err != nil {
		t.Fatal(err)
	}
	n, err := f.s.ClearExpiredWebhookSecrets(ctx)
	if err != nil || n != 1 {
		t.Fatalf("cleared %d, %v; want 1", n, err)
	}
	if ep, _ := f.s.GetWebhookEndpointByID(ctx, f.alerts.ID); ep.PrevSecret != "" || ep.PrevExpiresAt != nil {
		t.Errorf("expired previous secret kept: %+v", ep)
	}
	if ep, _ := f.s.GetWebhookEndpointByID(ctx, f.guard.ID); ep.PrevSecret != secretB {
		t.Errorf("a secret still inside its window was cleared: %+v", ep)
	}
	if n, err := f.s.ClearExpiredWebhookSecrets(ctx); err != nil || n != 0 {
		t.Errorf("second pass cleared %d, %v", n, err)
	}
}

func TestReleaseWebhookDeliveriesGivesTheAttemptBack(t *testing.T) {
	ctx := context.Background()
	f := newWebhookFixture(t)
	f.enqueue(t, f.alerts, "a")
	f.enqueue(t, f.alerts, "b")
	got, _, err := f.s.ClaimWebhookDeliveries(ctx, 10, time.Hour)
	if err != nil || len(got) != 2 {
		t.Fatalf("claim: %d, %v", len(got), err)
	}
	if err := f.s.ReleaseWebhookDeliveries(ctx, []int64{got[0].Delivery.ID}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.ReleaseWebhookDeliveries(ctx, nil); err != nil {
		t.Errorf("empty release: %v", err)
	}
	again, _, err := f.s.ClaimWebhookDeliveries(ctx, 10, time.Hour)
	if err != nil || len(again) != 1 || again[0].Delivery.ID != got[0].Delivery.ID || again[0].Delivery.Attempts != 1 {
		t.Errorf("after release: %d, %v; want the released delivery again at attempt 1", len(again), err)
	}
}
