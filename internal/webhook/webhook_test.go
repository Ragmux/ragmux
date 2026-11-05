package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/ragmux/ragmux/internal/metrics"
	"github.com/ragmux/ragmux/internal/obs"
	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/internal/testdb"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// ---- signing ---------------------------------------------------------------

// The vector published with the Standard Webhooks specification.
func TestSignatureMatchesTheSpecVector(t *testing.T) {
	got, err := Signature("whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw", "msg_p5jXN8AQM9LWM0D4loKWxJek", 1614265330, []byte(`{"test": 2432232314}`))
	if err != nil {
		t.Fatal(err)
	}
	if want := "v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE="; got != want {
		t.Errorf("signature = %q, want %q", got, want)
	}
}

// verify is a receiver written from the specification, independent of Sign.
func verify(t *testing.T, secret string, h http.Header, body []byte) bool {
	t.Helper()
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_"))
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(h.Get("webhook-id") + "." + h.Get("webhook-timestamp") + "."))
	mac.Write(body)
	want := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	for _, part := range strings.Fields(h.Get("webhook-signature")) {
		v, sig, ok := strings.Cut(part, ",")
		if ok && v == "v1" && hmac.Equal([]byte(sig), []byte(want)) {
			return true
		}
	}
	return false
}

func TestGenerateSecretFormat(t *testing.T) {
	a, err := GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := GenerateSecret()
	if a == b {
		t.Error("two secrets are equal")
	}
	raw, ok := strings.CutPrefix(a, "whsec_")
	if !ok {
		t.Fatalf("secret %q lacks the whsec_ prefix", a)
	}
	key, err := base64.StdEncoding.DecodeString(raw)
	if err != nil || len(key) != 32 {
		t.Errorf("secret body decodes to %d bytes, err %v; want 32", len(key), err)
	}
}

func TestSignatureRejectsMalformedSecret(t *testing.T) {
	for _, s := range []string{"", "plain", "whsec_", "whsec_!!!not-base64"} {
		if _, err := Signature(s, "id", 1, nil); err == nil {
			t.Errorf("secret %q accepted", s)
		}
	}
	if _, err := SignatureHeader(nil, "id", 1, nil); err == nil {
		t.Error("no secrets accepted")
	}
}

func TestEventIDIsDeterministicAndScoped(t *testing.T) {
	a := EventID(7, "limit:rpm:2026-11-04T10")
	if a != EventID(7, "limit:rpm:2026-11-04T10") {
		t.Error("same endpoint and key gave different ids")
	}
	if a == EventID(8, "limit:rpm:2026-11-04T10") || a == EventID(7, "limit:rpm:2026-11-04T11") {
		t.Error("different endpoint or key gave the same id")
	}
	if !strings.HasPrefix(a, "evt_") || len(a) != len("evt_")+24 {
		t.Errorf("id %q is not evt_ + 24 hex", a)
	}
}

func TestActiveSecretsFollowTheRotationWindow(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	exp := now.Add(time.Hour)
	ep := &store.WebhookEndpoint{Secret: "whsec_new", PrevSecret: "whsec_old", PrevExpiresAt: &exp}
	if got := ActiveSecrets(ep, now); len(got) != 2 || got[0] != "whsec_new" || got[1] != "whsec_old" {
		t.Errorf("inside the window = %v, want [new old]", got)
	}
	if got := ActiveSecrets(ep, exp); len(got) != 1 || got[0] != "whsec_new" {
		t.Errorf("at expiry = %v, want [new]", got)
	}
	if got := ActiveSecrets(&store.WebhookEndpoint{Secret: "whsec_new"}, now); len(got) != 1 {
		t.Errorf("no previous secret = %v, want one", got)
	}
}

func TestRetryDelaySchedule(t *testing.T) {
	base := []time.Duration{5 * time.Second, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour}
	for i, want := range base {
		attempt := i + 1
		if got := RetryDelay(attempt, 0.5); got != want {
			t.Errorf("attempt %d, centred jitter = %v, want %v", attempt, got, want)
		}
		lo, hi := RetryDelay(attempt, 0), RetryDelay(attempt, 0.999999)
		if lo != time.Duration(float64(want)*0.9) || hi < time.Duration(float64(want)*1.0999) || hi > time.Duration(float64(want)*1.1) {
			t.Errorf("attempt %d jitter bounds = [%v, %v] around %v, want +-10%%", attempt, lo, hi, want)
		}
	}
	for _, attempt := range []int{0, MaxAttempts, MaxAttempts + 1, -1} {
		if got := RetryDelay(attempt, 0.5); got != 0 {
			t.Errorf("attempt %d = %v, want 0 (no attempt left)", attempt, got)
		}
	}
}

// ---- URL rules -------------------------------------------------------------

func TestValidateURL(t *testing.T) {
	c := NewClient(map[string]bool{"127.0.0.1": true, "internal.guard": true})
	long := "https://example.com/" + strings.Repeat("a", MaxURLBytes)
	cases := []struct {
		name, url string
		ok        bool
	}{
		{"http to an allowlisted loopback host", "http://127.0.0.1:9000/hook", true},
		{"https to an allowlisted loopback host", "https://127.0.0.1/hook", true},
		{"http to a host that is not allowlisted", "http://example.com/hook", false},
		{"userinfo", "https://user:pass@example.com/hook", false},
		{"userinfo without password", "https://user@example.com/hook", false},
		{"ftp", "ftp://example.com/hook", false},
		{"no scheme", "example.com/hook", false},
		{"empty", "", false},
		{"too long", long, false},
		{"loopback literal, https, not allowlisted", "https://127.0.0.2/hook", false},
		{"private literal", "https://10.0.0.5/hook", false},
		{"cloud metadata literal", "https://169.254.169.254/latest/meta-data", false},
		{"ipv6 loopback literal", "https://[::1]/hook", false},
		{"localhost", "https://localhost/hook", false},
		{"http to an allowlisted hostname that does not resolve is not refused here", "http://internal.guard/hook", true},
	}
	for _, tc := range cases {
		err := c.ValidateURL(context.Background(), tc.url)
		if tc.ok && err != nil {
			t.Errorf("%s: %v", tc.name, err)
		}
		if !tc.ok {
			if err == nil {
				t.Errorf("%s: accepted", tc.name)
			} else if !errors.Is(err, ErrInvalidURL) {
				t.Errorf("%s: error %v does not wrap ErrInvalidURL", tc.name, err)
			}
		}
	}
	// The message names the webhook list, never the upstream one.
	err := c.ValidateURL(context.Background(), "https://10.0.0.5/hook")
	if err == nil || !strings.Contains(err.Error(), "PRIVATE_WEBHOOK_ALLOWLIST") || strings.Contains(err.Error(), "PRIVATE_UPSTREAM_ALLOWLIST") {
		t.Errorf("message = %v", err)
	}
}

// ---- sending ---------------------------------------------------------------

type recorded struct {
	header http.Header
	body   []byte
}

// receiver is a test webhook endpoint that records what it got.
func receiver(t *testing.T, status int, reply string) (*httptest.Server, func() []recorded) {
	t.Helper()
	var mu sync.Mutex
	var got []recorded
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, recorded{header: r.Header.Clone(), body: b})
		mu.Unlock()
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []recorded {
		mu.Lock()
		defer mu.Unlock()
		return append([]recorded(nil), got...)
	}
}

func loopbackClient(opts ...Option) *Client {
	return NewClient(map[string]bool{"127.0.0.1": true}, opts...)
}

func mustSecret(t *testing.T) string {
	t.Helper()
	s, err := GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestCallSendsASignedStandardWebhook(t *testing.T) {
	srv, got := receiver(t, 200, `{"action":"allow"}`)
	secret := mustSecret(t)
	clock := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	c := loopbackClient(WithVersion("9.9.9"), WithClock(func() time.Time { return clock }))
	body := []byte(`{"text":"hello"}`)

	resp, err := c.Call(context.Background(), Request{
		Purpose: "guardrail", URL: srv.URL + "/g", EventID: "evt_abc", Body: body,
		Secrets: []string{secret}, Timeout: 2 * time.Second, MaxResponseBytes: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || string(resp.Body) != `{"action":"allow"}` {
		t.Errorf("response = %d %q", resp.StatusCode, resp.Body)
	}
	reqs := got()
	if len(reqs) != 1 {
		t.Fatalf("%d requests, want 1", len(reqs))
	}
	h := reqs[0].header
	if h.Get("webhook-id") != "evt_abc" || h.Get("webhook-timestamp") != strconv.FormatInt(clock.Unix(), 10) {
		t.Errorf("id/timestamp = %q/%q", h.Get("webhook-id"), h.Get("webhook-timestamp"))
	}
	if h.Get("User-Agent") != "ragmux/9.9.9" || h.Get("Content-Type") != "application/json" {
		t.Errorf("user agent/content type = %q/%q", h.Get("User-Agent"), h.Get("Content-Type"))
	}
	if string(reqs[0].body) != string(body) {
		t.Errorf("body = %q", reqs[0].body)
	}
	if !verify(t, secret, h, reqs[0].body) {
		t.Errorf("signature %q does not verify with the secret", h.Get("webhook-signature"))
	}
	if n := len(strings.Fields(h.Get("webhook-signature"))); n != 1 {
		t.Errorf("%d signatures outside a rotation window, want 1", n)
	}
	// A tampered body must not verify.
	if verify(t, secret, h, []byte(`{"text":"HELLO"}`)) {
		t.Error("signature verified a different body")
	}
}

func TestCallSignsWithBothSecretsInARotationWindow(t *testing.T) {
	srv, got := receiver(t, 200, "")
	oldSecret, newSecret := mustSecret(t), mustSecret(t)
	c := loopbackClient()
	req := Request{Purpose: "alerts", URL: srv.URL, EventID: "evt_1", Body: []byte(`{}`), Secrets: []string{newSecret, oldSecret}}
	if _, err := c.Call(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	h := got()[0].header
	if n := len(strings.Fields(h.Get("webhook-signature"))); n != 2 {
		t.Fatalf("%d signatures in the window, want 2: %q", n, h.Get("webhook-signature"))
	}
	if !verify(t, newSecret, h, []byte(`{}`)) || !verify(t, oldSecret, h, []byte(`{}`)) {
		t.Error("each secret must verify one of the signatures")
	}
	// After the window only the new secret signs.
	req.Secrets = []string{newSecret}
	if _, err := c.Call(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	h = got()[1].header
	if n := len(strings.Fields(h.Get("webhook-signature"))); n != 1 || verify(t, oldSecret, h, []byte(`{}`)) {
		t.Errorf("after the window: %d signatures, old secret verifies=%v", n, verify(t, oldSecret, h, []byte(`{}`)))
	}
}

func TestCallRefusesPrivateTargetsUnlessAllowlisted(t *testing.T) {
	srv, got := receiver(t, 200, "")
	secret := mustSecret(t)
	req := Request{Purpose: "guardrail", URL: srv.URL, EventID: "e", Body: []byte(`{}`), Secrets: []string{secret}}

	strict := NewClient(nil) // https only, no private host
	_, err := strict.Call(context.Background(), req)
	var ce *CallError
	if !errors.As(err, &ce) || ce.Class != store.DeliveryErrBlocked {
		t.Errorf("http to loopback: %v, want class blocked", err)
	}

	// https to a loopback literal passes the URL rules but is refused at dial.
	httpsReq := req
	httpsReq.URL = "https://" + strings.TrimPrefix(srv.URL, "http://")
	_, err = strict.Call(context.Background(), httpsReq)
	if !errors.As(err, &ce) || ce.Class != store.DeliveryErrBlocked {
		t.Errorf("https to loopback: %v, want class blocked", err)
	}
	if n := len(got()); n != 0 {
		t.Fatalf("%d request(s) reached the loopback receiver through the guard", n)
	}

	// ALLOW_PRIVATE_UPSTREAMS semantics do not exist here: only the host list lifts it.
	if _, err := loopbackClient().Call(context.Background(), req); err != nil {
		t.Errorf("allowlisted host refused: %v", err)
	}
	if n := len(got()); n != 1 {
		t.Errorf("%d requests, want 1 after allowlisting", n)
	}
}

func TestCallDoesNotFollowRedirects(t *testing.T) {
	target, hits := receiver(t, 200, "")
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirector.Close()
	resp, err := loopbackClient().Call(context.Background(), Request{
		Purpose: "alerts", URL: redirector.URL, EventID: "e", Body: []byte(`{}`), Secrets: []string{mustSecret(t)},
	})
	var ce *CallError
	if !errors.As(err, &ce) || ce.StatusCode != http.StatusFound || ce.Class != store.DeliveryErrHTTP4xx {
		t.Fatalf("redirect: err = %v, want http_4xx with status 302", err)
	}
	if resp.StatusCode != http.StatusFound {
		t.Errorf("status = %d, want 302", resp.StatusCode)
	}
	if n := len(hits()); n != 0 {
		t.Errorf("redirect target was requested %d time(s)", n)
	}
}

func TestCallClassifiesFailures(t *testing.T) {
	secret := mustSecret(t)
	call := func(c *Client, rawURL string, timeout time.Duration) error {
		_, err := c.Call(context.Background(), Request{Purpose: "alerts", URL: rawURL, EventID: "e", Body: []byte(`{}`), Secrets: []string{secret}, Timeout: timeout})
		return err
	}
	class := func(err error) string {
		var ce *CallError
		if !errors.As(err, &ce) {
			return fmt.Sprintf("not a CallError: %v", err)
		}
		return ce.Class
	}

	for status, want := range map[int]string{400: "http_4xx", 404: "http_4xx", 410: "http_4xx", 429: "http_4xx", 500: "http_5xx", 503: "http_5xx"} {
		srv, _ := receiver(t, status, "upstream says secret-token-123")
		err := call(loopbackClient(), srv.URL, 0)
		if class(err) != want {
			t.Errorf("status %d: class %q, want %q", status, class(err), want)
		}
		// The error text never carries the receiver's reply.
		if strings.Contains(err.Error(), "secret-token-123") {
			t.Errorf("status %d: error text leaks the response body: %v", status, err)
		}
	}

	// timeout
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	}))
	defer slow.Close()
	if got := class(call(loopbackClient(), slow.URL, 100*time.Millisecond)); got != store.DeliveryErrTimeout {
		t.Errorf("slow receiver: class %q, want timeout", got)
	}

	// network: a port nobody listens on.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	if got := class(call(loopbackClient(), "http://"+addr, time.Second)); got != store.DeliveryErrNetwork {
		t.Errorf("closed port: class %q, want network", got)
	}
}

func TestCallKeepsOnlyTheRequestedPartOfTheReply(t *testing.T) {
	srv, _ := receiver(t, 200, strings.Repeat("x", 10_000))
	resp, err := loopbackClient().Call(context.Background(), Request{
		Purpose: "guardrail", URL: srv.URL, EventID: "e", Body: []byte(`{}`), Secrets: []string{mustSecret(t)}, MaxResponseBytes: 100,
	})
	if err != nil || len(resp.Body) != 100 {
		t.Errorf("body %d bytes, err %v; want 100", len(resp.Body), err)
	}
}

func TestCallCountsItsOutcome(t *testing.T) {
	reg := metrics.New(metrics.Options{})
	c := loopbackClient(WithMetrics(obs.New(reg)))
	ok, _ := receiver(t, 200, "")
	bad, _ := receiver(t, 500, "")
	secret := mustSecret(t)
	for _, u := range []string{ok.URL, bad.URL} {
		_, _ = c.Call(context.Background(), Request{Purpose: "guardrail", URL: u, EventID: "e", Body: []byte(`{}`), Secrets: []string{secret}})
	}
	text := reg.Text()
	for _, want := range []string{`outcome="delivered"`, `outcome="failed"`, `purpose="guardrail"`} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics lack %s:\n%s", want, text)
		}
	}
	if strings.Contains(text, "127.0.0.1") {
		t.Error("the URL reached a metric label")
	}
}

// ---- DNS rebinding ---------------------------------------------------------

// fakeDNS answers every A query with the address held in ip and serves
// net.DefaultResolver for the duration of the test.
func fakeDNS(t *testing.T) (set func(ip net.IP)) {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot listen for a fake DNS server: %v", err)
	}
	var current atomic.Value
	current.Store(net.IPv4(93, 184, 216, 34))
	go func() {
		buf := make([]byte, 512)
		for {
			n, from, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			var p dnsmessage.Parser
			h, err := p.Start(buf[:n])
			if err != nil {
				continue
			}
			q, err := p.Question()
			if err != nil {
				continue
			}
			b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: h.ID, Response: true, Authoritative: true, RecursionAvailable: true})
			b.EnableCompression()
			_ = b.StartQuestions()
			_ = b.Question(q)
			_ = b.StartAnswers()
			if q.Type == dnsmessage.TypeA {
				var a [4]byte
				copy(a[:], current.Load().(net.IP).To4())
				_ = b.AResource(dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: 0}, dnsmessage.AResource{A: a})
			}
			if msg, err := b.Finish(); err == nil {
				_, _ = conn.WriteTo(msg, from)
			}
		}
	}()
	prev := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "udp", conn.LocalAddr().String())
	}}
	t.Cleanup(func() {
		net.DefaultResolver = prev
		_ = conn.Close()
	})
	return func(ip net.IP) { current.Store(ip) }
}

// A name that resolves to a public address when the URL is registered and to
// a loopback address when the delivery is made is caught at the connection.
func TestDNSRebindingIsCaughtAtConnectionTime(t *testing.T) {
	setIP := fakeDNS(t)
	srv, got := receiver(t, 200, "")
	port := srv.Listener.Addr().(*net.TCPAddr).Port
	rawURL := fmt.Sprintf("http://rebind.test:%d/hook", port)
	secret := mustSecret(t)
	req := Request{Purpose: "alerts", URL: rawURL, EventID: "e", Body: []byte(`{}`), Secrets: []string{secret}, Timeout: 2 * time.Second}

	// Not allowlisted: http is refused outright, so exercise https for the
	// registration check and the dial.
	strict := NewClient(nil)
	httpsURL := fmt.Sprintf("https://rebind.test:%d/hook", port)
	if err := strict.ValidateURL(context.Background(), httpsURL); err != nil {
		t.Skipf("fake DNS is not serving this resolver (%v); cannot prove rebinding here", err)
	}
	setIP(net.IPv4(127, 0, 0, 1)) // the "rebind"
	req.URL = httpsURL
	_, err := strict.Call(context.Background(), req)
	var ce *CallError
	if !errors.As(err, &ce) || ce.Class != store.DeliveryErrBlocked {
		t.Fatalf("rebound name: %v, want class blocked", err)
	}
	if n := len(got()); n != 0 {
		t.Fatalf("%d request(s) reached the loopback receiver after the rebind", n)
	}
	// Re-registration now fails as well.
	if err := strict.ValidateURL(context.Background(), httpsURL); err == nil {
		t.Error("a name that resolves to loopback was accepted at registration")
	}

	// The allowlisted spelling of the same name connects (http allowed there).
	allowed := NewClient(map[string]bool{"rebind.test": true})
	req.URL = rawURL
	if _, err := allowed.Call(context.Background(), req); err != nil {
		t.Errorf("allowlisted rebind.test: %v", err)
	}
	if n := len(got()); n != 1 {
		t.Errorf("%d requests through the allowlisted name, want 1", n)
	}
}

// ---- worker (in-memory store) ---------------------------------------------

type fakeStore struct {
	mu         sync.Mutex
	clock      time.Time
	deliveries map[int64]*store.WebhookDelivery
	endpoints  map[int64]*store.WebhookEndpoint
	disabled   []int64
	released   []int64
	// dropped is what the next claim reports as failed by the store.
	dropped map[string]int
	// disableBlocks makes DisableWebhookEndpoint hang until its context ends.
	disableBlocks bool
	disableErr    error
	claims        int
}

func newFakeStore(now time.Time) *fakeStore {
	return &fakeStore{clock: now, deliveries: map[int64]*store.WebhookDelivery{}, endpoints: map[int64]*store.WebhookEndpoint{}}
}

func (f *fakeStore) add(ep *store.WebhookEndpoint, d *store.WebhookDelivery) {
	f.endpoints[ep.ID] = ep
	d.Status = store.DeliveryPending
	d.NextAttemptAt = f.clock
	f.deliveries[d.ID] = d
}

func (f *fakeStore) ClaimWebhookDeliveries(_ context.Context, limit int, lease time.Duration) ([]store.ClaimedWebhookDelivery, map[string]int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claims++
	var out []store.ClaimedWebhookDelivery
	for id := int64(1); id < 100 && len(out) < limit; id++ {
		d, ok := f.deliveries[id]
		if !ok || d.Status != store.DeliveryPending || d.NextAttemptAt.After(f.clock) || f.endpoints[d.EndpointID].DisabledAt != nil {
			continue
		}
		d.Attempts++
		d.NextAttemptAt = f.clock.Add(lease)
		out = append(out, store.ClaimedWebhookDelivery{Delivery: *d, Endpoint: f.endpoints[d.EndpointID]})
	}
	return out, f.dropped, nil
}

func (f *fakeStore) FinishWebhookDelivery(_ context.Context, id int64, r store.WebhookDeliveryResult) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := f.deliveries[id]
	if d == nil || d.Status != store.DeliveryPending {
		return store.ErrNotFound
	}
	d.Status = r.Status
	if r.Status == store.DeliveryPending {
		d.NextAttemptAt = f.clock.Add(r.RetryIn)
	}
	d.LastStatus = r.HTTPStatus
	d.LastErrorClass = nil
	if r.ErrorClass != "" {
		c := r.ErrorClass
		d.LastErrorClass = &c
	}
	ms := r.DurationMS
	d.LastDurationMS = &ms
	return nil
}

func (f *fakeStore) DisableWebhookEndpoint(ctx context.Context, id int64) error {
	if f.disableBlocks {
		<-ctx.Done()
		return ctx.Err()
	}
	if f.disableErr != nil {
		return f.disableErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.clock
	f.endpoints[id].DisabledAt = &now
	f.disabled = append(f.disabled, id)
	for _, d := range f.deliveries {
		if d.EndpointID == id && d.Status == store.DeliveryPending {
			d.Status = store.DeliveryFailed
		}
	}
	return nil
}

func (f *fakeStore) ReleaseWebhookDeliveries(_ context.Context, ids []int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range ids {
		if d := f.deliveries[id]; d != nil && d.Status == store.DeliveryPending {
			d.Attempts = max(d.Attempts-1, 0)
			d.NextAttemptAt = f.clock
			f.released = append(f.released, id)
		}
	}
	return nil
}

func (f *fakeStore) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clock = f.clock.Add(d)
}

func (f *fakeStore) delivery(id int64) store.WebhookDelivery {
	f.mu.Lock()
	defer f.mu.Unlock()
	return *f.deliveries[id]
}

type workerRig struct {
	st     *fakeStore
	w      *Worker
	reg    *metrics.Registry
	secret string
}

func newWorkerRig(t *testing.T, targetURL string, payload string) *workerRig {
	t.Helper()
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	st := newFakeStore(now)
	secret := mustSecret(t)
	st.add(&store.WebhookEndpoint{ID: 1, ProjectID: 9, Purpose: "alerts", URL: targetURL, Secret: secret},
		&store.WebhookDelivery{ID: 1, EndpointID: 1, EventType: "limit.warning", DedupeKey: "rpm:80:2026-11-04T12", Payload: []byte(payload)})
	reg := metrics.New(metrics.Options{})
	w := NewWorker(st, loopbackClient(), WithLogger(quietLog()), WithWorkerMetrics(obs.New(reg)),
		WithWorkerClock(func() time.Time { return st.clock }), WithJitter(func() float64 { return 0.5 }),
		WithAttemptTimeout(2*time.Second))
	return &workerRig{st: st, w: w, reg: reg, secret: secret}
}

func TestWorkerDeliversAndKeepsTheWebhookIDAcrossRetries(t *testing.T) {
	var calls atomic.Int32
	var mu sync.Mutex
	var ids, bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		ids = append(ids, r.Header.Get("webhook-id"))
		bodies = append(bodies, string(b))
		mu.Unlock()
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	rig := newWorkerRig(t, srv.URL, `{"limit":"rpm","ratio":0.8}`)
	ctx := context.Background()

	// attempt 1 -> 502, retry in 5 s
	if n, err := rig.w.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("poll 1: n=%d err=%v", n, err)
	}
	d := rig.st.delivery(1)
	if d.Status != store.DeliveryPending || d.Attempts != 1 || d.LastStatus == nil || *d.LastStatus != 502 ||
		d.LastErrorClass == nil || *d.LastErrorClass != "http_5xx" {
		t.Fatalf("after attempt 1: %+v", d)
	}
	if want := rig.st.clock.Add(5 * time.Second); !d.NextAttemptAt.Equal(want) {
		t.Errorf("next attempt = %v, want %v", d.NextAttemptAt, want)
	}
	// not due yet: nothing claimed
	if n, _ := rig.w.RunOnce(ctx); n != 0 {
		t.Errorf("a delivery that is not due was claimed (%d)", n)
	}
	rig.st.advance(5 * time.Second)
	if n, _ := rig.w.RunOnce(ctx); n != 1 {
		t.Fatalf("poll 2 claimed %d", n)
	}
	d = rig.st.delivery(1)
	if want := rig.st.clock.Add(5 * time.Minute); d.Status != store.DeliveryPending || !d.NextAttemptAt.Equal(want) {
		t.Fatalf("after attempt 2: status %s next %v want %v", d.Status, d.NextAttemptAt, want)
	}
	rig.st.advance(5 * time.Minute)
	if n, _ := rig.w.RunOnce(ctx); n != 1 {
		t.Fatalf("poll 3 claimed %d", n)
	}
	d = rig.st.delivery(1)
	if d.Status != store.DeliveryDelivered || d.Attempts != 3 || d.LastErrorClass != nil || d.LastStatus == nil || *d.LastStatus != 204 {
		t.Fatalf("after attempt 3: %+v", d)
	}

	mu.Lock()
	defer mu.Unlock()
	want := EventID(1, "rpm:80:2026-11-04T12")
	for i, id := range ids {
		if id != want {
			t.Errorf("attempt %d webhook-id = %q, want %q (must not change on retry)", i+1, id, want)
		}
		if bodies[i] != `{"limit":"rpm","ratio":0.8}` {
			t.Errorf("attempt %d body = %q", i+1, bodies[i])
		}
	}
	text := rig.reg.Text()
	for _, want := range []string{`outcome="retry"`, `outcome="delivered"`} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics lack %s:\n%s", want, text)
		}
	}
}

func TestWorkerFailsAfterFiveAttemptsOnTheScheduleAndStoresNoBody(t *testing.T) {
	srv, got := receiver(t, 503, "stack trace with secret-token-123")
	rig := newWorkerRig(t, srv.URL, `{}`)
	ctx := context.Background()
	wantDelay := []time.Duration{5 * time.Second, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour}
	for attempt := 1; attempt <= MaxAttempts; attempt++ {
		if n, err := rig.w.RunOnce(ctx); err != nil || n != 1 {
			t.Fatalf("attempt %d: n=%d err=%v", attempt, n, err)
		}
		d := rig.st.delivery(1)
		if attempt < MaxAttempts {
			if d.Status != store.DeliveryPending {
				t.Fatalf("attempt %d: status %s, want pending", attempt, d.Status)
			}
			if want := rig.st.clock.Add(wantDelay[attempt-1]); !d.NextAttemptAt.Equal(want) {
				t.Fatalf("attempt %d: next %v, want %v", attempt, d.NextAttemptAt, want)
			}
			rig.st.advance(wantDelay[attempt-1])
		}
	}
	d := rig.st.delivery(1)
	if d.Status != store.DeliveryFailed || d.Attempts != MaxAttempts {
		t.Fatalf("final: status %s attempts %d, want failed/%d", d.Status, d.Attempts, MaxAttempts)
	}
	if d.LastErrorClass == nil || *d.LastErrorClass != "http_5xx" {
		t.Errorf("class = %v", d.LastErrorClass)
	}
	if n := len(got()); n != MaxAttempts {
		t.Errorf("%d requests, want %d", n, MaxAttempts)
	}
	// Nothing left to claim, and the row is not retried.
	rig.st.advance(24 * time.Hour)
	if n, _ := rig.w.RunOnce(ctx); n != 0 {
		t.Errorf("a failed delivery was claimed again (%d)", n)
	}
	if dump := fmt.Sprintf("%+v", d); strings.Contains(dump, "secret-token-123") {
		t.Error("the receiver's reply was stored")
	}
}

func TestWorkerDisablesTheEndpointOn410(t *testing.T) {
	srv, got := receiver(t, http.StatusGone, "")
	rig := newWorkerRig(t, srv.URL, `{}`)
	// A second delivery for the same endpoint must be failed, not sent.
	rig.st.add(rig.st.endpoints[1], &store.WebhookDelivery{ID: 2, EndpointID: 1, EventType: "limit.warning", DedupeKey: "other", Payload: []byte(`{}`)})
	rig.st.deliveries[2].NextAttemptAt = rig.st.clock.Add(time.Hour)

	if n, _ := rig.w.RunOnce(context.Background()); n != 1 {
		t.Fatalf("claimed %d, want the one due delivery", n)
	}
	d := rig.st.delivery(1)
	if d.Status != store.DeliveryFailed || d.LastStatus == nil || *d.LastStatus != 410 || d.LastErrorClass == nil || *d.LastErrorClass != "http_4xx" {
		t.Errorf("410 delivery: %+v", d)
	}
	if len(rig.st.disabled) != 1 || rig.st.disabled[0] != 1 || rig.st.endpoints[1].DisabledAt == nil {
		t.Errorf("endpoint not disabled: %v", rig.st.disabled)
	}
	if d2 := rig.st.delivery(2); d2.Status != store.DeliveryFailed {
		t.Errorf("the endpoint's other pending delivery is %s, want failed", d2.Status)
	}
	rig.st.advance(2 * time.Hour)
	if n, _ := rig.w.RunOnce(context.Background()); n != 0 || len(got()) != 1 {
		t.Errorf("after 410: claimed %d, receiver saw %d request(s); want 0 and 1", n, len(got()))
	}
	if text := rig.reg.Text(); !strings.Contains(text, `outcome="disabled"`) {
		t.Errorf("no disabled outcome:\n%s", text)
	}
}

func TestWorkerSignsWithThePreviousSecretOnlyInsideTheWindow(t *testing.T) {
	srv, got := receiver(t, 200, "")
	rig := newWorkerRig(t, srv.URL, `{}`)
	prev := mustSecret(t)
	exp := rig.st.clock.Add(time.Hour)
	ep := rig.st.endpoints[1]
	ep.PrevSecret, ep.PrevExpiresAt = prev, &exp
	rig.st.add(ep, &store.WebhookDelivery{ID: 2, EndpointID: 1, EventType: "e", DedupeKey: "later", Payload: []byte(`{}`)})
	rig.st.deliveries[2].NextAttemptAt = rig.st.clock.Add(2 * time.Hour)

	_, _ = rig.w.RunOnce(context.Background()) // inside the window
	rig.st.advance(2 * time.Hour)
	_, _ = rig.w.RunOnce(context.Background()) // after it

	reqs := got()
	if len(reqs) != 2 {
		t.Fatalf("%d requests, want 2", len(reqs))
	}
	if n := len(strings.Fields(reqs[0].header.Get("webhook-signature"))); n != 2 || !verify(t, prev, reqs[0].header, reqs[0].body) || !verify(t, rig.secret, reqs[0].header, reqs[0].body) {
		t.Errorf("inside the window: %d signatures, both secrets must verify", n)
	}
	if n := len(strings.Fields(reqs[1].header.Get("webhook-signature"))); n != 1 || verify(t, prev, reqs[1].header, reqs[1].body) || !verify(t, rig.secret, reqs[1].header, reqs[1].body) {
		t.Errorf("after the window: %d signatures, only the current secret may verify", n)
	}
}

func TestWorkerFailsADeliveryWhoseAttemptsRanOutInACrash(t *testing.T) {
	srv, got := receiver(t, 200, "")
	rig := newWorkerRig(t, srv.URL, `{}`)
	rig.st.deliveries[1].Attempts = MaxAttempts // the previous worker died during attempt 5
	if n, _ := rig.w.RunOnce(context.Background()); n != 1 {
		t.Fatalf("claimed %d", n)
	}
	if d := rig.st.delivery(1); d.Status != store.DeliveryFailed {
		t.Errorf("status %s, want failed", d.Status)
	}
	if len(got()) != 0 {
		t.Error("a sixth attempt was sent")
	}
}

func TestWorkerRefusesAStoredURLThatTheGuardNoLongerAllows(t *testing.T) {
	srv, got := receiver(t, 200, "")
	rig := newWorkerRig(t, srv.URL, `{}`)
	// Same worker, but a client whose allowlist no longer has the host.
	rig.w.client = NewClient(nil)
	if n, _ := rig.w.RunOnce(context.Background()); n != 1 {
		t.Fatalf("claimed %d", n)
	}
	d := rig.st.delivery(1)
	if d.Status != store.DeliveryPending || d.LastErrorClass == nil || *d.LastErrorClass != "blocked" {
		t.Errorf("delivery = %+v, want pending with class blocked", d)
	}
	if len(got()) != 0 {
		t.Error("the receiver was contacted")
	}
}

func TestWorkerRunStopsOnCancel(t *testing.T) {
	srv, _ := receiver(t, 200, "")
	rig := newWorkerRig(t, srv.URL, `{}`)
	w := NewWorker(rig.st, loopbackClient(), WithLogger(quietLog()), WithPollInterval(10*time.Millisecond),
		WithWorkerClock(func() time.Time { return rig.st.clock }))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	deadline := time.After(5 * time.Second)
	for rig.st.delivery(1).Status != store.DeliveryDelivered {
		select {
		case <-deadline:
			t.Fatal("Run did not deliver")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// guard against a regression where a URL is mistaken for a label or log field.
func TestCallErrorTextNeverHasTheURL(t *testing.T) {
	_, err := NewClient(nil).Call(context.Background(), Request{
		Purpose: "alerts", URL: "https://10.1.2.3/very-secret-path?token=abc", EventID: "e", Body: []byte(`{}`), Secrets: []string{mustSecret(t)},
	})
	if err == nil {
		t.Fatal("expected a failure")
	}
	if u, _ := url.Parse("https://10.1.2.3/very-secret-path?token=abc"); strings.Contains(err.Error(), u.Path) || strings.Contains(err.Error(), "token=abc") {
		t.Errorf("error text carries the URL: %v", err)
	}
}

// ---- worker against the real store (needs TEST_DATABASE_URL) ---------------

type dbRig struct {
	st     *store.Store
	ep     *store.WebhookEndpoint
	secret string
}

func newDBRig(t *testing.T, targetURL string) *dbRig {
	t.Helper()
	ctx := context.Background()
	st := testdb.Open(t)
	conn, err := st.CreateConnection(ctx, &store.ModelConnection{Name: "c", ProviderType: "openai", APIKey: "k", ModelName: "m"})
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := st.CreateProject(ctx, &store.Project{Name: "p", ModelConnectionID: conn.ID})
	if err != nil {
		t.Fatal(err)
	}
	secret := mustSecret(t)
	ep, err := st.CreateWebhookEndpoint(ctx, &store.WebhookEndpoint{ProjectID: p.ID, Purpose: store.WebhookPurposeAlerts, URL: targetURL, Secret: secret})
	if err != nil {
		t.Fatal(err)
	}
	return &dbRig{st: st, ep: ep, secret: secret}
}

func (r *dbRig) worker(opts ...WorkerOption) *Worker {
	return NewWorker(r.st, loopbackClient(), append([]WorkerOption{WithLogger(quietLog()), WithAttemptTimeout(2 * time.Second)}, opts...)...)
}

func (r *dbRig) deliveries(t *testing.T) []store.WebhookDelivery {
	t.Helper()
	list, err := r.st.ListWebhookDeliveries(context.Background(), r.ep.ID, 500)
	if err != nil {
		t.Fatal(err)
	}
	return list
}

// The same dedupe key is one delivery with one webhook-id, however many times
// the alert source asks for it.
func TestOutboxDedupeGivesOneDeliveryAndOneWebhookID(t *testing.T) {
	srv, got := receiver(t, 200, "")
	rig := newDBRig(t, srv.URL)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := rig.st.EnqueueWebhookDelivery(ctx, rig.ep.ID, "limit.warning", "rpm:80:2026-11-04T12", []byte(`{"limit":"rpm"}`)); err != nil {
			t.Fatal(err)
		}
	}
	w := rig.worker()
	for i := 0; i < 3; i++ {
		if _, err := w.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	reqs := got()
	if len(reqs) != 1 {
		t.Fatalf("%d requests for one deduplicated event, want 1", len(reqs))
	}
	if id := reqs[0].header.Get("webhook-id"); id != EventID(rig.ep.ID, "rpm:80:2026-11-04T12") {
		t.Errorf("webhook-id = %q", id)
	}
	if !verify(t, rig.secret, reqs[0].header, reqs[0].body) {
		t.Error("signature does not verify")
	}
	list := rig.deliveries(t)
	if len(list) != 1 || list[0].Status != store.DeliveryDelivered || list[0].Attempts != 1 {
		t.Errorf("deliveries = %+v", list)
	}
}

func TestSynchronousCallLeavesNoDeliveryRow(t *testing.T) {
	srv, _ := receiver(t, 200, `{"action":"allow"}`)
	rig := newDBRig(t, srv.URL)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		_, err := loopbackClient().Call(ctx, Request{Purpose: "guardrail", URL: srv.URL, EventID: fmt.Sprintf("evt_%d", i),
			Body: []byte(`{"text":"private prompt"}`), Secrets: []string{rig.secret}, MaxResponseBytes: 1024})
		if err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := rig.st.DB().QueryRow(ctx, "SELECT COUNT(*) FROM webhook_deliveries").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d delivery rows after synchronous calls, want 0 (they carry request content)", n)
	}
}

// Several workers polling at once send each delivery exactly once.
func TestTwoWorkersNeverSendTheSameDelivery(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.Header.Get("webhook-id")]++
		mu.Unlock()
	}))
	defer srv.Close()
	rig := newDBRig(t, srv.URL)
	ctx := context.Background()
	const total = 60
	for i := 0; i < total; i++ {
		if _, err := rig.st.EnqueueWebhookDelivery(ctx, rig.ep.ID, "limit.warning", fmt.Sprintf("k%d", i), []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		w := rig.worker(WithBatchSize(10), WithConcurrency(4))
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				n, err := w.RunOnce(ctx)
				if err != nil {
					t.Error(err)
					return
				}
				if n == 0 {
					return
				}
			}
		}()
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != total {
		t.Errorf("%d distinct events delivered, want %d", len(seen), total)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("event %s delivered %d times", id, n)
		}
	}
}

func TestWorkerOn410ClosesTheEndpointInTheDatabase(t *testing.T) {
	srv, got := receiver(t, http.StatusGone, "")
	rig := newDBRig(t, srv.URL)
	ctx := context.Background()
	for _, k := range []string{"a", "b"} {
		if _, err := rig.st.EnqueueWebhookDelivery(ctx, rig.ep.ID, "limit.warning", k, []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	// One at a time, so the second is still pending when the first gets 410.
	w := rig.worker(WithBatchSize(1))
	if n, err := w.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	ep, err := rig.st.GetWebhookEndpointByID(ctx, rig.ep.ID)
	if err != nil || ep.DisabledAt == nil {
		t.Fatalf("endpoint after 410: %+v, %v", ep, err)
	}
	for _, d := range rig.deliveries(t) {
		if d.Status != store.DeliveryFailed {
			t.Errorf("delivery %s is %s, want failed", d.DedupeKey, d.Status)
		}
	}
	if ok, err := rig.st.EnqueueWebhookDelivery(ctx, rig.ep.ID, "limit.warning", "c", []byte(`{}`)); err != nil || ok {
		t.Errorf("enqueue on a disabled endpoint: ok=%v err=%v", ok, err)
	}
	if n, _ := w.RunOnce(ctx); n != 0 || len(got()) != 1 {
		t.Errorf("claimed %d after 410, receiver saw %d request(s); want 0 and 1", n, len(got()))
	}
}

// What is stored about a failure is the closed class and the status, nothing
// the receiver said.
func TestStoredFailureIsAClassNotAMessage(t *testing.T) {
	srv, _ := receiver(t, 500, "Traceback: password=hunter2 at /srv/app.py")
	rig := newDBRig(t, srv.URL)
	ctx := context.Background()
	if _, err := rig.st.EnqueueWebhookDelivery(ctx, rig.ep.ID, "limit.warning", "a", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := rig.worker().RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var dump string
	if err := rig.st.DB().QueryRow(ctx, "SELECT d::text FROM webhook_deliveries d").Scan(&dump); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(dump, "hunter2") || strings.Contains(dump, "Traceback") {
		t.Errorf("the receiver's reply was stored: %s", dump)
	}
	d := rig.deliveries(t)[0]
	if d.LastErrorClass == nil || *d.LastErrorClass != "http_5xx" || d.LastStatus == nil || *d.LastStatus != 500 || d.Status != store.DeliveryPending {
		t.Errorf("delivery = %+v", d)
	}
}

// ---- review fixes ------------------------------------------------------------

func TestEventIDSeparatesEndpointFromKey(t *testing.T) {
	if EventID(1, "23:x") == EventID(12, "3:x") {
		t.Error("endpoint 1 with key 23:x and endpoint 12 with key 3:x share a webhook-id")
	}
}

// An environment proxy would dial on our behalf and take the address check
// with it; the transport must never consult one.
func TestClientTransportHasNoProxy(t *testing.T) {
	tr, ok := NewClient(nil).http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T", NewClient(nil).http.Transport)
	}
	if tr.Proxy != nil {
		t.Error("the transport consults a proxy")
	}
	if tr.DialContext == nil {
		t.Error("the transport has no guarded dialer")
	}
}

// A reply cut short is the failure it is, not a success with a short body.
func TestCallReportsAReplyThatStallsMidBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "partial")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	}))
	defer srv.Close()
	reg := metrics.New(metrics.Options{})
	c := loopbackClient(WithMetrics(obs.New(reg)))
	_, err := c.Call(context.Background(), Request{Purpose: "guardrail", URL: srv.URL, EventID: "e", Body: []byte(`{}`),
		Secrets: []string{mustSecret(t)}, Timeout: 200 * time.Millisecond, MaxResponseBytes: 100})
	var ce *CallError
	if !errors.As(err, &ce) || ce.Class != store.DeliveryErrTimeout {
		t.Fatalf("stalled body: %v, want class timeout", err)
	}
	if text := reg.Text(); strings.Contains(text, `outcome="delivered"`) || !strings.Contains(text, `outcome="failed"`) {
		t.Errorf("a cut-short reply was counted as delivered:\n%s", text)
	}
}

// After cancellation no new attempt starts and the claims not yet begun go
// back to the queue with their attempt.
func TestWorkerStopsStartingAttemptsOnCancelAndReleasesTheRest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		cancel() // the shutdown signal arrives while the first attempt is in flight
	}))
	defer srv.Close()
	rig := newWorkerRig(t, srv.URL, `{}`)
	ep := rig.st.endpoints[1]
	for id := int64(2); id <= 8; id++ {
		rig.st.add(ep, &store.WebhookDelivery{ID: id, EndpointID: 1, EventType: "e", DedupeKey: fmt.Sprintf("k%d", id), Payload: []byte(`{}`)})
	}
	w := NewWorker(rig.st, loopbackClient(), WithLogger(quietLog()), WithConcurrency(1), WithAttemptTimeout(2*time.Second))

	n, err := w.RunOnce(ctx)
	if err != nil || n != 8 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("%d attempts started after the signal, want only the one in flight", got)
	}
	if d := rig.st.delivery(1); d.Status != store.DeliveryDelivered {
		t.Errorf("the in-flight delivery is %s, want recorded as delivered", d.Status)
	}
	if len(rig.st.released) != 7 {
		t.Fatalf("%d claims released, want 7", len(rig.st.released))
	}
	for id := int64(2); id <= 8; id++ {
		if d := rig.st.delivery(id); d.Status != store.DeliveryPending || d.Attempts != 0 || d.NextAttemptAt.After(rig.st.clock) {
			t.Errorf("delivery %d = status %s attempts %d next %v; want pending, attempt given back, due now", id, d.Status, d.Attempts, d.NextAttemptAt)
		}
	}
}

// A database that stops answering must not hang the worker on a 410.
func TestWorkerBoundsTheDisableCall(t *testing.T) {
	srv, _ := receiver(t, http.StatusGone, "")
	rig := newWorkerRig(t, srv.URL, `{}`)
	rig.st.disableBlocks = true
	rig.w.storeTimeout = 100 * time.Millisecond
	done := make(chan struct{})
	go func() { _, _ = rig.w.RunOnce(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunOnce hangs on a database call without a deadline")
	}
}

// Deliveries the store failed for a damaged endpoint never reach deliver; the
// worker still counts them as failed.
func TestWorkerCountsDeliveriesTheStoreDropped(t *testing.T) {
	srv, got := receiver(t, 200, "")
	rig := newWorkerRig(t, srv.URL, `{}`)
	rig.st.dropped = map[string]int{"alerts": 3, "guardrail": 1}
	if _, err := rig.w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	text := rig.reg.Text()
	for _, want := range []string{
		`ragmux_webhook_deliveries_total{purpose="alerts",outcome="failed"} 3`,
		`ragmux_webhook_deliveries_total{purpose="guardrail",outcome="failed"} 1`,
		`ragmux_webhook_deliveries_total{purpose="alerts",outcome="delivered"} 1`, // the healthy one
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics lack %s:\n%s", want, text)
		}
	}
	if len(got()) != 1 {
		t.Errorf("the healthy delivery was not sent (%d requests)", len(got()))
	}
}

// A 410 whose disable write fails leaves the endpoint open; it must not be
// logged or counted as disabled.
func TestWorkerDoesNotClaimDisabledWhenTheWriteFails(t *testing.T) {
	srv, _ := receiver(t, http.StatusGone, "")
	rig := newWorkerRig(t, srv.URL, `{}`)
	rig.st.disableErr = errors.New("connection reset")
	var buf strings.Builder
	rig.w.log = slog.New(slog.NewTextHandler(&buf, nil))
	if _, err := rig.w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	text := rig.reg.Text()
	if strings.Contains(text, `outcome="disabled"`) {
		t.Error("a failed disable was counted as disabled")
	}
	// The delivery was failed for good by the 410; that shows in the metric.
	if !strings.Contains(text, `ragmux_webhook_deliveries_total{purpose="alerts",outcome="failed"} 1`) {
		t.Errorf("the failed delivery was not counted:\n%s", text)
	}
	if log := buf.String(); strings.Contains(log, "and was disabled") || !strings.Contains(log, "could not be disabled") {
		t.Errorf("log = %s", log)
	}
}
