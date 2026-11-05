// Package webhook is the outbound HTTP core shared by the guardrail webhook
// (synchronous, request content in the body) and limit alerts (asynchronous,
// through the Postgres outbox). ADR-033.
//
// Every request is signed the Standard Webhooks way (v1, HMAC-SHA256), goes
// out over a transport whose dialer refuses private, loopback and link-local
// addresses unless the host is on PRIVATE_WEBHOOK_ALLOWLIST, never follows a
// redirect, and reads at most a few KB of the reply. Nothing here logs or
// stores a URL, a secret or a receiver's response body.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	mrand "math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ragmux/ragmux/internal/netguard"
	"github.com/ragmux/ragmux/internal/obs"
	"github.com/ragmux/ragmux/internal/store"
)

// Limits and defaults of the core.
const (
	// MaxURLBytes is the longest webhook URL accepted.
	MaxURLBytes = 2048
	// DefaultTimeout bounds one attempt when the caller gives none.
	DefaultTimeout = 5 * time.Second
	// DiscardBytes is how much of a reply an asynchronous attempt reads before
	// dropping the rest: enough to let the connection be reused, never stored.
	DiscardBytes = 4 << 10
	// SecretPrefix starts every webhook secret; the rest is base64 of the key.
	SecretPrefix = "whsec_"
	// RotationOverlap is how long the previous secret keeps signing after a
	// rotation, so a receiver can switch without dropping a delivery.
	RotationOverlap = 24 * time.Hour
)

// ErrInvalidURL is wrapped by every URL validation failure. Its message is
// written for the person who typed the URL and is safe to show (HTTP 400).
var ErrInvalidURL = errors.New("invalid webhook url")

// GenerateSecret returns a new signing secret: "whsec_" plus the base64 of 32
// random bytes. It is shown to the user once, at creation and at rotation.
func GenerateSecret() (string, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return "", err
	}
	return SecretPrefix + base64.StdEncoding.EncodeToString(key), nil
}

// EventID is the deterministic webhook-id of a deduplicated event:
// "evt_" + hex(SHA-256(endpoint_id ":" dedupe_key))[:24]. A retry and a
// duplicate detected by the receiver carry the same id.
func EventID(endpointID int64, dedupeKey string) string {
	h := sha256.New()
	h.Write([]byte(strconv.FormatInt(endpointID, 10)))
	// A decimal id never contains ':', so "1"+"23:x" and "12"+"3:x" differ.
	h.Write([]byte{':'})
	h.Write([]byte(dedupeKey))
	return "evt_" + hex.EncodeToString(h.Sum(nil))[:24]
}

// signingKey decodes a "whsec_" secret to the HMAC key, as every Standard
// Webhooks verifier does.
func signingKey(secret string) ([]byte, error) {
	raw, ok := strings.CutPrefix(secret, SecretPrefix)
	if !ok {
		return nil, errors.New("webhook secret does not start with " + SecretPrefix)
	}
	key, err := base64.StdEncoding.DecodeString(raw)
	if err != nil || len(key) == 0 {
		return nil, errors.New("webhook secret is not valid base64")
	}
	return key, nil
}

// Signature returns one "v1,<base64(HMAC-SHA256(id.timestamp.body))>" value.
func Signature(secret, id string, timestamp int64, body []byte) (string, error) {
	key, err := signingKey(secret)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id))
	mac.Write([]byte{'.'})
	mac.Write([]byte(strconv.FormatInt(timestamp, 10)))
	mac.Write([]byte{'.'})
	mac.Write(body)
	return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil)), nil
}

// SignatureHeader joins the signatures of secrets with a space, the form the
// webhook-signature header takes during a rotation window.
func SignatureHeader(secrets []string, id string, timestamp int64, body []byte) (string, error) {
	if len(secrets) == 0 {
		return "", errors.New("webhook has no secret to sign with")
	}
	sigs := make([]string, 0, len(secrets))
	for _, s := range secrets {
		sig, err := Signature(s, id, timestamp, body)
		if err != nil {
			return "", err
		}
		sigs = append(sigs, sig)
	}
	return strings.Join(sigs, " "), nil
}

// ActiveSecrets returns the secrets an endpoint signs with at now: the
// current one, then the previous one while its rotation window is open.
func ActiveSecrets(e *store.WebhookEndpoint, now time.Time) []string {
	out := []string{e.Secret}
	if e.PrevSecret != "" && e.PrevExpiresAt != nil && now.Before(*e.PrevExpiresAt) {
		out = append(out, e.PrevSecret)
	}
	return out
}

// Client sends signed webhooks. Build it once with NewClient and share it: the
// synchronous guardrail call and the asynchronous worker use the same one.
type Client struct {
	http      *http.Client
	allowlist map[string]bool
	version   string
	metrics   *obs.Metrics
	now       func() time.Time
}

// Option customises a Client.
type Option func(*Client)

// WithVersion sets the version in the User-Agent ("ragmux/<version>").
func WithVersion(v string) Option { return func(c *Client) { c.version = v } }

// WithMetrics makes synchronous Call count its outcome in
// ragmux_webhook_deliveries_total. The worker counts its own attempts.
func WithMetrics(m *obs.Metrics) Option { return func(c *Client) { c.metrics = m } }

// WithClock replaces the clock the timestamp header is read from (tests).
func WithClock(now func() time.Time) Option { return func(c *Client) { c.now = now } }

// NewClient builds a Client whose transport dials through
// netguard.SafeDialContext with allowPrivate=false: a host may reach a private
// address only if it is listed in allowlist (PRIVATE_WEBHOOK_ALLOWLIST, lower
// case). The check runs on every connection, on the addresses just resolved,
// so a DNS answer cannot be swapped for a private one after the URL was
// validated. The transport has no proxy and the client follows no redirect.
func NewClient(allowlist map[string]bool, opts ...Option) *Client {
	c := &Client{allowlist: allowlist, version: "dev", now: time.Now}
	for _, o := range opts {
		o(c)
	}
	dialer := &net.Dialer{Timeout: DefaultTimeout}
	transport := &http.Transport{
		// Proxy stays nil: an environment proxy would dial for us and take
		// the address check with it.
		Proxy:                 nil,
		DialContext:           netguard.SafeDialContext(dialer, false, allowlist),
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   DefaultTimeout,
		ResponseHeaderTimeout: 0, // the per-call context bounds the whole attempt
		MaxIdleConns:          16,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       30 * time.Second,
	}
	c.http = &http.Client{
		Transport: transport,
		// The 3xx itself is the answer; it is reported as an error.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return c
}

// hostAllowed reports whether host is on the private-webhook allowlist.
func (c *Client) hostAllowed(host string) bool {
	return c.allowlist[strings.ToLower(strings.TrimSuffix(host, "."))]
}

// parseURL applies the static rules of a webhook URL.
func (c *Client) parseURL(raw string) (*url.URL, error) {
	if len(raw) == 0 || len(raw) > MaxURLBytes {
		return nil, fmt.Errorf("%w: it must be 1 to %d bytes", ErrInvalidURL, MaxURLBytes)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Hostname() == "" {
		return nil, fmt.Errorf("%w: it must be an absolute http(s) URL", ErrInvalidURL)
	}
	if u.User != nil {
		return nil, fmt.Errorf("%w: credentials in the URL are not allowed", ErrInvalidURL)
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
	case "http":
		if !c.hostAllowed(u.Hostname()) {
			return nil, fmt.Errorf("%w: http is only allowed for hosts on PRIVATE_WEBHOOK_ALLOWLIST; use https", ErrInvalidURL)
		}
	default:
		return nil, fmt.Errorf("%w: the scheme must be https", ErrInvalidURL)
	}
	return u, nil
}

// ValidateURL is the registration-time check: the static rules of parseURL
// plus a resolution of the host that must not land only on private addresses
// (netguard.CheckHost). The dial-time check stays authoritative; this one only
// turns an obviously forbidden URL into a 400 instead of a delivery that never
// succeeds. Errors wrap ErrInvalidURL.
func (c *Client) ValidateURL(ctx context.Context, raw string) error {
	u, err := c.parseURL(raw)
	if err != nil {
		return err
	}
	if err := netguard.CheckHost(ctx, u.Hostname(), false, c.allowlist); err != nil {
		return fmt.Errorf("%w: host %q resolves to a private or local address; add it to PRIVATE_WEBHOOK_ALLOWLIST to allow it",
			ErrInvalidURL, u.Hostname())
	}
	return nil
}

// Request is one signed POST.
type Request struct {
	// Purpose is "guardrail" or "alerts"; it labels the metric.
	Purpose string
	URL     string
	// EventID is the webhook-id header: the same value on every retry of one
	// event. Synchronous callers generate a fresh one per call.
	EventID string
	Body    []byte
	// Secrets sign the body, current first (see ActiveSecrets).
	Secrets []string
	// Timeout bounds the attempt end to end; zero means DefaultTimeout.
	Timeout time.Duration
	// MaxResponseBytes caps how much of the reply is returned in
	// Response.Body; the rest is dropped.
	MaxResponseBytes int
}

// Response is what an attempt got back.
type Response struct {
	// StatusCode is 0 when no response arrived.
	StatusCode int
	// Body holds at most Request.MaxResponseBytes of the reply. Callers that
	// persist anything must not persist it (ADR-033).
	Body     []byte
	Duration time.Duration
}

// CallError is the failure of an attempt. Class is one of the store.DeliveryErr*
// values, the only thing that may be stored or shown about the failure.
type CallError struct {
	Class      string
	StatusCode int
	err        error
}

func (e *CallError) Error() string {
	if e.StatusCode != 0 {
		return fmt.Sprintf("webhook: %s (status %d)", e.Class, e.StatusCode)
	}
	return "webhook: " + e.Class
}

// Unwrap exposes the transport error for errors.Is/As in the same process; it
// is never serialised.
func (e *CallError) Unwrap() error { return e.err }

// classify maps a transport error to the closed error-class set.
func classify(err error) string {
	var ne *netguard.Error
	if errors.As(err, &ne) {
		return store.DeliveryErrBlocked
	}
	var nerr net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &nerr) && nerr.Timeout()) {
		return store.DeliveryErrTimeout
	}
	return store.DeliveryErrNetwork
}

// statusClass maps a non-2xx status to its class: 5xx is http_5xx, everything
// else below 600 (4xx, and the 3xx we refuse to follow) is http_4xx.
func statusClass(code int) string {
	if code >= 500 {
		return store.DeliveryErrHTTP5xx
	}
	return store.DeliveryErrHTTP4xx
}

// Call is the synchronous API: one signed POST, the reply returned, no retry,
// nothing written to the database (the guardrail body carries request
// content). The caller bounds it with Request.Timeout and ctx. A non-2xx
// answer is an error (*CallError) and the Response is still filled so the
// caller can see the status. The outcome is counted when the client was built
// WithMetrics.
func (c *Client) Call(ctx context.Context, req Request) (Response, error) {
	resp, err := c.do(ctx, req, req.MaxResponseBytes)
	outcome := obs.WebhookDelivered
	if err != nil {
		outcome = obs.WebhookFailed
	}
	c.metrics.RecordWebhookDelivery(req.Purpose, outcome)
	return resp, err
}

// do performs one attempt. maxBody is how much of the reply to keep; anything
// beyond it is read up to DiscardBytes and dropped.
func (c *Client) do(ctx context.Context, req Request, maxBody int) (Response, error) {
	start := time.Now()
	fail := func(class string, err error) (Response, error) {
		return Response{Duration: time.Since(start)}, &CallError{Class: class, err: err}
	}
	u, err := c.parseURL(req.URL)
	if err != nil {
		// A stored URL that no longer satisfies the rules (the allowlist
		// shrank, say) is refused like a blocked dial.
		return fail(store.DeliveryErrBlocked, err)
	}
	ts := c.now().Unix()
	sig, err := SignatureHeader(req.Secrets, req.EventID, ts, req.Body)
	if err != nil {
		return fail(store.DeliveryErrNetwork, err)
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(req.Body))
	if err != nil {
		return fail(store.DeliveryErrNetwork, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", "ragmux/"+c.version)
	httpReq.Header.Set("webhook-id", req.EventID)
	httpReq.Header.Set("webhook-timestamp", strconv.FormatInt(ts, 10))
	httpReq.Header.Set("webhook-signature", sig)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return fail(classify(err), err)
	}
	defer func() { _ = resp.Body.Close() }()
	var kept []byte
	if maxBody > 0 {
		var rerr error
		if kept, rerr = io.ReadAll(io.LimitReader(resp.Body, int64(maxBody))); rerr != nil {
			// A reply cut short (timeout, reset) is not a success the caller
			// can parse; report it under the class it belongs to.
			return Response{StatusCode: resp.StatusCode, Duration: time.Since(start)},
				&CallError{Class: classify(rerr), StatusCode: resp.StatusCode, err: rerr}
		}
	}
	// Let the connection be reused without reading an unbounded reply.
	_, _ = io.CopyN(io.Discard, resp.Body, DiscardBytes)
	out := Response{StatusCode: resp.StatusCode, Body: kept, Duration: time.Since(start)}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return out, &CallError{Class: statusClass(resp.StatusCode), StatusCode: resp.StatusCode}
	}
	return out, nil
}

// Delivery schedule (ADR-033): attempt 1 is immediate, the next ones follow
// retryDelays after the attempt before them, with +-10% jitter. After
// MaxAttempts failed attempts the delivery is failed.
const (
	MaxAttempts = 5
	// RetryJitter is the fraction the delay may deviate by in either direction.
	RetryJitter = 0.10
)

var retryDelays = [MaxAttempts - 1]time.Duration{5 * time.Second, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour}

// RetryDelay is how long to wait after the failed attempt number attempt
// (1-based) before the next one. unit is a uniform value in [0,1) that picks
// the jitter, so tests can pin it. It returns 0 once no attempt is left.
func RetryDelay(attempt int, unit float64) time.Duration {
	if attempt < 1 || attempt > len(retryDelays) {
		return 0
	}
	base := retryDelays[attempt-1]
	factor := 1 + RetryJitter*(2*unit-1)
	return time.Duration(float64(base) * factor)
}

// Worker defaults.
const (
	DefaultPollInterval = 2 * time.Second
	DefaultBatchSize    = 20
	DefaultConcurrency  = 4
	// DefaultLease is how long a claimed delivery is reserved for its worker
	// before another may take it over: well above the time one batch needs
	// (20 deliveries, 4 at a time, 5 s each: 25 s), and below the 5 minute
	// delay of the second retry.
	DefaultLease = 2 * time.Minute
)

// DeliveryStore is the part of *store.Store the worker needs.
type DeliveryStore interface {
	ClaimWebhookDeliveries(ctx context.Context, limit int, lease time.Duration) ([]store.ClaimedWebhookDelivery, map[string]int, error)
	FinishWebhookDelivery(ctx context.Context, id int64, r store.WebhookDeliveryResult) error
	DisableWebhookEndpoint(ctx context.Context, id int64) error
	ReleaseWebhookDeliveries(ctx context.Context, ids []int64) error
}

// Worker drains the webhook_deliveries outbox. Run one per replica: the claim
// is FOR UPDATE SKIP LOCKED, so replicas and goroutines split the work.
type Worker struct {
	store       DeliveryStore
	client      *Client
	log         *slog.Logger
	metrics     *obs.Metrics
	poll        time.Duration
	batch       int
	concurrency int
	lease       time.Duration
	timeout     time.Duration
	// storeTimeout bounds each database write that follows an attempt.
	storeTimeout time.Duration
	now          func() time.Time
	unit         func() float64
}

// WorkerOption customises a Worker.
type WorkerOption func(*Worker)

// WithLogger sets the worker's logger.
func WithLogger(l *slog.Logger) WorkerOption { return func(w *Worker) { w.log = l } }

// WithWorkerMetrics makes the worker count every attempt.
func WithWorkerMetrics(m *obs.Metrics) WorkerOption { return func(w *Worker) { w.metrics = m } }

// WithPollInterval sets the pause between polls (default 2 s).
func WithPollInterval(d time.Duration) WorkerOption {
	return func(w *Worker) {
		if d > 0 {
			w.poll = d
		}
	}
}

// WithConcurrency sets how many deliveries one replica sends at once
// (default 4).
func WithConcurrency(n int) WorkerOption {
	return func(w *Worker) {
		if n > 0 {
			w.concurrency = n
		}
	}
}

// WithBatchSize sets how many deliveries one poll claims (default 20).
func WithBatchSize(n int) WorkerOption {
	return func(w *Worker) {
		if n > 0 {
			w.batch = n
		}
	}
}

// WithAttemptTimeout sets the per-attempt timeout (default 5 s).
func WithAttemptTimeout(d time.Duration) WorkerOption {
	return func(w *Worker) {
		if d > 0 {
			w.timeout = d
		}
	}
}

// WithWorkerClock replaces the clock the rotation window is read from (tests).
func WithWorkerClock(now func() time.Time) WorkerOption { return func(w *Worker) { w.now = now } }

// WithJitter replaces the jitter source with a function returning [0,1) (tests).
func WithJitter(unit func() float64) WorkerOption { return func(w *Worker) { w.unit = unit } }

// NewWorker builds the delivery worker. Start it with Run.
func NewWorker(st DeliveryStore, client *Client, opts ...WorkerOption) *Worker {
	w := &Worker{
		store: st, client: client, log: slog.Default(),
		poll: DefaultPollInterval, batch: DefaultBatchSize, concurrency: DefaultConcurrency,
		lease: DefaultLease, timeout: DefaultTimeout, storeTimeout: 10 * time.Second, now: time.Now, unit: mrand.Float64,
	}
	for _, o := range opts {
		o(w)
	}
	return w
}

// Run polls the outbox until ctx is cancelled. A poll that fills a whole batch
// is followed by the next one at once; otherwise the worker waits the poll
// interval. On cancellation it stops claiming and returns after the
// deliveries already in flight have finished and been recorded (each is
// bounded by the attempt timeout).
func (w *Worker) Run(ctx context.Context) {
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		n, err := w.RunOnce(ctx)
		if err != nil && ctx.Err() == nil {
			w.log.Warn("webhook worker poll failed", "err", err)
		}
		wait := w.poll
		if err == nil && n >= w.batch {
			wait = 0
		}
		t.Reset(wait)
	}
}

// RunOnce claims one batch and delivers it, returning how many deliveries it
// claimed. It returns after every attempt it started has been recorded.
//
// Once ctx is cancelled no further attempt is started: the claims not yet
// begun are handed back (attempt given back, due at once) so a replica that
// is shutting down neither sends for longer than the attempts already in
// flight need (each is bounded by the attempt timeout) nor leaves rows leased.
func (w *Worker) RunOnce(ctx context.Context) (int, error) {
	claimed, dropped, err := w.store.ClaimWebhookDeliveries(ctx, w.batch, w.lease)
	if err != nil {
		return 0, err
	}
	// Deliveries the store failed because their endpoint's secret is damaged
	// never reach deliver; count them so the loss shows in the metric.
	for purpose, n := range dropped {
		for range n {
			w.metrics.RecordWebhookDelivery(purpose, obs.WebhookFailed)
		}
	}
	// An attempt that has started is finished and recorded even when ctx is
	// cancelled meanwhile: the row is already leased and its attempt counted,
	// so dropping the result would only burn the attempt.
	detached := context.WithoutCancel(ctx)
	sem := make(chan struct{}, w.concurrency)
	var wg sync.WaitGroup
	var unstarted []int64
	for _, c := range claimed {
		sem <- struct{}{}
		if ctx.Err() != nil {
			<-sem
			unstarted = append(unstarted, c.Delivery.ID)
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			w.deliver(detached, c)
		}()
	}
	wg.Wait()
	if len(unstarted) > 0 {
		rctx, cancel := context.WithTimeout(detached, w.storeTimeout)
		defer cancel()
		if err := w.store.ReleaseWebhookDeliveries(rctx, unstarted); err != nil {
			w.log.Warn("unstarted webhook deliveries not released; they retry when their lease ends", "count", len(unstarted), "err", err)
		}
	}
	return len(claimed), nil
}

// deliver makes one attempt on a claimed delivery and records the result.
func (w *Worker) deliver(ctx context.Context, c store.ClaimedWebhookDelivery) {
	d, ep := c.Delivery, c.Endpoint
	record := func(outcome string) { w.metrics.RecordWebhookDelivery(ep.Purpose, outcome) }
	finish := func(r store.WebhookDeliveryResult) {
		fctx, cancel := context.WithTimeout(ctx, w.storeTimeout)
		defer cancel()
		if err := w.store.FinishWebhookDelivery(fctx, d.ID, r); err != nil && !errors.Is(err, store.ErrNotFound) {
			w.log.Warn("webhook delivery result not recorded", "delivery", d.ID, "endpoint", ep.ID, "err", err)
		}
	}

	// A claim past the last attempt means the previous worker died during it;
	// nothing was sent this time.
	if d.Attempts > MaxAttempts {
		finish(store.WebhookDeliveryResult{Status: store.DeliveryFailed, HTTPStatus: d.LastStatus, ErrorClass: deref(d.LastErrorClass), DurationMS: derefInt(d.LastDurationMS)})
		record(obs.WebhookFailed)
		return
	}

	resp, err := w.client.do(ctx, Request{
		Purpose: ep.Purpose,
		URL:     ep.URL,
		EventID: EventID(ep.ID, d.DedupeKey),
		Body:    d.Payload,
		Secrets: ActiveSecrets(ep, w.now()),
		Timeout: w.timeout,
	}, 0)
	ms := int(resp.Duration / time.Millisecond)
	var status *int
	if resp.StatusCode != 0 {
		sc := resp.StatusCode
		status = &sc
	}

	if err == nil {
		finish(store.WebhookDeliveryResult{Status: store.DeliveryDelivered, HTTPStatus: status, DurationMS: ms})
		record(obs.WebhookDelivered)
		return
	}
	class := store.DeliveryErrNetwork
	var ce *CallError
	if errors.As(err, &ce) {
		class = ce.Class
	}
	res := store.WebhookDeliveryResult{HTTPStatus: status, ErrorClass: class, DurationMS: ms}

	if resp.StatusCode == http.StatusGone {
		res.Status = store.DeliveryFailed
		finish(res)
		dctx, dcancel := context.WithTimeout(ctx, w.storeTimeout)
		derr := w.store.DisableWebhookEndpoint(dctx, ep.ID)
		dcancel()
		if derr != nil && !errors.Is(derr, store.ErrNotFound) {
			// The endpoint is still open: say so, and do not count it as disabled.
			w.log.Warn("webhook endpoint answered 410 Gone but could not be disabled",
				"endpoint", ep.ID, "project", ep.ProjectID, "purpose", ep.Purpose, "err", derr)
			// The delivery itself is failed and final; the endpoint stays open.
			record(obs.WebhookFailed)
			return
		}
		w.log.Warn("webhook endpoint answered 410 Gone and was disabled",
			"endpoint", ep.ID, "project", ep.ProjectID, "purpose", ep.Purpose)
		record(obs.WebhookDisabled)
		return
	}
	if d.Attempts >= MaxAttempts {
		res.Status = store.DeliveryFailed
		finish(res)
		w.log.Warn("webhook delivery failed after the last attempt",
			"delivery", d.ID, "endpoint", ep.ID, "project", ep.ProjectID, "purpose", ep.Purpose, "class", class)
		record(obs.WebhookFailed)
		return
	}
	res.Status = store.DeliveryPending
	res.RetryIn = RetryDelay(d.Attempts, w.unit())
	finish(res)
	record(obs.WebhookRetry)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefInt(n *int) int {
	if n == nil {
		return 0
	}
	return *n
}
