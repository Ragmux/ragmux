package alerts_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ragmux/ragmux/internal/alerts"
	"github.com/ragmux/ragmux/internal/limits"
	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/internal/testdb"
	"github.com/ragmux/ragmux/internal/webhook"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// rig is a project with alerts on and an alerts endpoint, a job whose clock
// stands still, and helpers to spend and to read what was queued.
type rig struct {
	t   *testing.T
	st  *store.Store
	p   *store.Project
	ep  *store.WebhookEndpoint
	job *alerts.Job
	now time.Time
	w   store.UsageWindows
}

var start = time.Date(2026, 10, 7, 12, 30, 0, 0, time.UTC)

// newRig creates a project from p. usdBudget is its monthly money budget in
// USD millionths (0 = none). The endpoint URL is never called by the job.
func newRig(t *testing.T, p store.Project, usdBudget int64) *rig {
	t.Helper()
	ctx := context.Background()
	st := testdb.Open(t)
	conn, err := st.CreateConnection(ctx, &store.ModelConnection{Name: "m", ProviderType: "openai", ModelName: "gpt-4o"})
	if err != nil {
		t.Fatal(err)
	}
	p.Name, p.ModelConnectionID = "billing", conn.ID
	proj, _, err := st.CreateProject(ctx, &p)
	if err != nil {
		t.Fatal(err)
	}
	ep, err := st.CreateWebhookEndpoint(ctx, &store.WebhookEndpoint{ProjectID: proj.ID, Purpose: store.WebhookPurposeAlerts,
		URL: "http://127.0.0.1:9/hook", Secret: "whsec_test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetAlertSettings(ctx, proj.ID, true, usdBudget); err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, st: st, p: proj, ep: ep, now: start, w: limits.Windows(start)}
	// The rows were created "now" by the clock of the database, the tests live
	// in October: date the last edit before every window they use, so a late
	// pass sees limits that were in force. A test that edits a limit later
	// says when with edited.
	r.edited(start.AddDate(0, -2, 0))
	r.job = alerts.NewJob(st, alerts.WithLogger(quiet), alerts.WithClock(func() time.Time { return r.now }))
	return r
}

// edited dates the last edit of the project row and of its alert settings.
func (r *rig) edited(at time.Time) {
	r.t.Helper()
	ctx := context.Background()
	if _, err := r.st.DB().Exec(ctx, "UPDATE projects SET updated_at = $2 WHERE id = $1", r.p.ID, at); err != nil {
		r.t.Fatal(err)
	}
	if _, err := r.st.DB().Exec(ctx, "UPDATE project_alert_settings SET updated_at = $2 WHERE project_id = $1", r.p.ID, at); err != nil {
		r.t.Fatal(err)
	}
}

// use records one finished request on the project's counters.
func (r *rig) use(prompt, completion int, cost int64, unpriced bool) {
	r.t.Helper()
	if err := r.st.RecordUsage(context.Background(), r.p.ID, r.w, prompt, completion, cost, unpriced, true); err != nil {
		r.t.Fatal(err)
	}
}

// round runs one evaluation and insists this replica did it.
func (r *rig) round() {
	r.t.Helper()
	ran, err := r.job.RunOnce(context.Background())
	if err != nil || !ran {
		r.t.Fatalf("round: ran=%v err=%v", ran, err)
	}
}

type queued struct {
	d    store.WebhookDelivery
	body map[string]any
}

func (q queued) str(k string) string  { s, _ := q.body[k].(string); return s }
func (q queued) num(k string) float64 { f, _ := q.body[k].(float64); return f }

// queued lists the deliveries of the endpoint, oldest first.
func (r *rig) queued() []queued {
	r.t.Helper()
	ds, err := r.st.ListWebhookDeliveries(context.Background(), r.ep.ID, 500)
	if err != nil {
		r.t.Fatal(err)
	}
	slices.Reverse(ds)
	out := make([]queued, 0, len(ds))
	for _, d := range ds {
		q := queued{d: d, body: map[string]any{}}
		if err := json.Unmarshal(d.Payload, &q.body); err != nil {
			r.t.Fatalf("payload %s: %v", d.Payload, err)
		}
		out = append(out, q)
	}
	return out
}

// kinds renders what was queued as "type/limit/threshold" lines.
func kinds(qs []queued) []string {
	out := make([]string, 0, len(qs))
	for _, q := range qs {
		out = append(out, q.str("type")+"/"+q.str("limit")+"/"+strconv.Itoa(int(q.num("threshold"))))
	}
	return out
}

func want(t *testing.T, qs []queued, expected ...string) {
	t.Helper()
	got := kinds(qs)
	if !slices.Equal(got, expected) {
		t.Fatalf("queued %v, want %v", got, expected)
	}
}

func TestEachThresholdFiresOnceAcrossRounds(t *testing.T) {
	r := newRig(t, store.Project{BudgetDailyTokens: 1000}, 0)
	r.use(500, 200, 0, false) // 70 %
	r.round()
	want(t, r.queued())

	r.use(50, 50, 0, false) // 80 %
	for range 3 {
		r.round()
	}
	qs := r.queued()
	want(t, qs, "limit.threshold/budget_daily_tokens/80")

	q := qs[0]
	if q.str("id") != webhook.EventID(r.ep.ID, q.d.DedupeKey) {
		t.Errorf("payload id %q is not the webhook-id of the delivery (%q)", q.str("id"), webhook.EventID(r.ep.ID, q.d.DedupeKey))
	}
	if q.d.EventType != "limit.threshold" {
		t.Errorf("event_type = %q", q.d.EventType)
	}
	subj, _ := q.body["subject"].(map[string]any)
	proj, _ := q.body["project"].(map[string]any)
	if subj["kind"] != "project" || subj["id"] != strconv.FormatInt(r.p.ID, 10) ||
		proj["id"] != float64(r.p.ID) || proj["name"] != "billing" {
		t.Errorf("subject/project = %v / %v", subj, proj)
	}
	if q.num("used") != 800 || q.num("limit_value") != 1000 || q.num("ratio") != 0.8 {
		t.Errorf("used/limit/ratio = %v/%v/%v", q.num("used"), q.num("limit_value"), q.num("ratio"))
	}
	if q.str("window_start") != "2026-10-07T00:00:00Z" || q.str("window_end") != "2026-10-08T00:00:00Z" {
		t.Errorf("window = %s .. %s", q.str("window_start"), q.str("window_end"))
	}
	// A token event carries neither the estimate flag nor the unpriced count.
	for _, k := range []string{"estimate", "unpriced_requests"} {
		if _, ok := q.body[k]; ok {
			t.Errorf("token event has %q", k)
		}
	}

	r.use(150, 50, 0, false) // 1000: 100 %
	for range 3 {
		r.round()
	}
	want(t, r.queued(), "limit.threshold/budget_daily_tokens/80", "limit.threshold/budget_daily_tokens/100")

	// The next day is a new window: the same counters' fresh row warns again.
	r.now = r.now.Add(24 * time.Hour)
	r.w = limits.Windows(r.now)
	r.use(900, 0, 0, false)
	r.round()
	want(t, r.queued(), "limit.threshold/budget_daily_tokens/80", "limit.threshold/budget_daily_tokens/100",
		"limit.threshold/budget_daily_tokens/80")
}

// Both levels crossed in one round: each is sent, once.
func TestJumpPastBothThresholds(t *testing.T) {
	r := newRig(t, store.Project{BudgetMonthlyTokens: 1000}, 0)
	r.use(1100, 0, 0, false)
	r.round()
	r.round()
	want(t, r.queued(), "limit.threshold/budget_monthly_tokens/80", "limit.threshold/budget_monthly_tokens/100")
	if q := r.queued()[1]; q.str("window_start") != "2026-10-01T00:00:00Z" || q.str("window_end") != "2026-11-01T00:00:00Z" {
		t.Errorf("month window = %s .. %s", q.str("window_start"), q.str("window_end"))
	}
}

// The percentage is exact even where used*100 does not fit in 64 bits.
func TestThresholdMathIsExactAtHugeCounters(t *testing.T) {
	r := newRig(t, store.Project{BudgetMonthlyTokens: 9_000_000_000_000_000_000}, 0)
	r.use(7_200_000_000_000_000_000, 0, 0, false) // exactly 80 %
	r.round()
	want(t, r.queued(), "limit.threshold/budget_monthly_tokens/80")
	r.use(1_799_999_999_999_999_999, 0, 0, false) // one token short of 100 %
	r.round()
	want(t, r.queued(), "limit.threshold/budget_monthly_tokens/80")
	r.use(1, 0, 0, false)
	r.round()
	want(t, r.queued(), "limit.threshold/budget_monthly_tokens/80", "limit.threshold/budget_monthly_tokens/100")
}

func TestRaisedBudgetWarnsAgainWhenCrossedAgain(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, store.Project{BudgetDailyTokens: 1000}, 0)
	r.use(1000, 0, 0, false)
	r.round()
	want(t, r.queued(), "limit.threshold/budget_daily_tokens/80", "limit.threshold/budget_daily_tokens/100")

	// 1000 of 1200 is 83 %: the same window, a different limit.
	p := *r.p
	p.BudgetDailyTokens = 1200
	if _, err := r.st.UpdateProject(ctx, &p); err != nil {
		t.Fatal(err)
	}
	r.round()
	qs := r.queued()
	want(t, qs, "limit.threshold/budget_daily_tokens/80", "limit.threshold/budget_daily_tokens/100",
		"limit.threshold/budget_daily_tokens/80")
	if last := qs[2]; last.num("limit_value") != 1200 || last.num("used") != 1000 {
		t.Errorf("new event = limit %v used %v", last.num("limit_value"), last.num("used"))
	}
	r.round()
	if n := len(r.queued()); n != 3 {
		t.Errorf("a repeated round queued %d events, want 3 in all", n)
	}
}

func TestKeyBudgets(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, store.Project{}, 0)
	u, err := r.st.CreateUser(ctx, "owner", "h", "admin")
	if err != nil {
		t.Fatal(err)
	}
	mk := func(name string, daily int64) *store.APIKey {
		k, _, err := r.st.CreateAPIKey(ctx, &store.APIKey{Kind: store.KindGateway, Name: name, UserID: u.ID,
			BudgetDailyTokens: daily, ProjectIDs: []int64{r.p.ID}})
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	live, gone := mk("live", 100), mk("revoked", 100)
	if _, err := r.st.RevokeAPIKey(ctx, gone.ID); err != nil {
		t.Fatal(err)
	}
	for _, k := range []*store.APIKey{live, gone} {
		if err := r.st.RecordKeyUsage(ctx, k.ID, r.w, 90, 0, true); err != nil {
			t.Fatal(err)
		}
	}
	r.round()
	r.round()
	qs := r.queued()
	want(t, qs, "limit.threshold/budget_daily_tokens/80")
	subj, _ := qs[0].body["subject"].(map[string]any)
	if subj["kind"] != "key" || subj["id"] != strconv.FormatInt(live.ID, 10) {
		t.Errorf("subject = %v, want the live key %d", subj, live.ID)
	}
}

func TestEndUserWarnsAtTheCeilingOnlyAndRPMTPMNever(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, store.Project{EndUserDailyTokens: 100, EndUserRPM: 1, EndUserTPM: 5,
		RateLimitRPM: 1, RateLimitTPM: 5}, 0)
	rec := func(user string, tokens int64) {
		t.Helper()
		if err := r.st.RecordEndUserUsage(ctx, r.p.ID, user, r.w, tokens, 0, true); err != nil {
			t.Fatal(err)
		}
	}
	rec("carol", 85) // 85 %: an end user gets no 80 % notice
	r.use(1000, 1000, 0, false)
	r.round()
	want(t, r.queued()) // and the RPM/TPM limits, long exceeded, say nothing

	rec("carol", 15)
	rec("dave", 400)
	r.round()
	r.round()
	qs := r.queued()
	want(t, qs, "limit.threshold/budget_daily_tokens/100", "limit.threshold/budget_daily_tokens/100")
	ids := []string{}
	for _, q := range qs {
		subj, _ := q.body["subject"].(map[string]any)
		if subj["kind"] != "end_user" {
			t.Errorf("subject kind = %v", subj["kind"])
		}
		ids = append(ids, subj["id"].(string))
	}
	slices.Sort(ids)
	if !slices.Equal(ids, []string{"carol", "dave"}) {
		t.Errorf("subjects = %v", ids)
	}
}

func TestMoneyBudgetWarnsAndStaysAnEstimate(t *testing.T) {
	r := newRig(t, store.Project{}, 1_000_000) // one dollar a month
	r.use(10, 10, 700_000, false)
	r.round()
	want(t, r.queued())

	r.use(10, 10, 100_000, true) // 80 %, and a request without a price
	r.use(10, 10, 0, true)
	r.round()
	qs := r.queued()
	want(t, qs, "limit.threshold/budget_monthly_usd/80", "budget.unpriced_traffic/budget_monthly_usd/0")
	m := qs[0]
	if m.num("used") != 800_000 || m.num("limit_value") != 1_000_000 || m.num("ratio") != 0.8 || m.num("threshold") != 80 {
		t.Errorf("money event = %v", m.body)
	}
	if m.body["estimate"] != true || m.num("unpriced_requests") != 2 {
		t.Errorf("money event: estimate=%v unpriced_requests=%v; want true and 2", m.body["estimate"], m.num("unpriced_requests"))
	}
	if m.str("window_start") != "2026-10-01T00:00:00Z" || m.str("window_end") != "2026-11-01T00:00:00Z" {
		t.Errorf("money window = %s .. %s", m.str("window_start"), m.str("window_end"))
	}

	r.use(10, 10, 300_000, false) // 100 %
	r.round()
	r.round()
	want(t, r.queued(), "limit.threshold/budget_monthly_usd/80", "budget.unpriced_traffic/budget_monthly_usd/0",
		"limit.threshold/budget_monthly_usd/100")
	if last := r.queued()[2]; last.num("used") != 1_100_000 || last.num("unpriced_requests") != 2 {
		t.Errorf("100 %% event: used %v, unpriced %v", last.num("used"), last.num("unpriced_requests"))
	}
}

func TestUnpricedTrafficOncePerMonthWithoutLimitFields(t *testing.T) {
	r := newRig(t, store.Project{}, 5_000_000)
	r.round() // nothing unpriced yet
	want(t, r.queued())

	r.use(10, 10, 0, true)
	for range 3 {
		r.round()
	}
	r.use(10, 10, 0, true)
	r.round()
	qs := r.queued()
	want(t, qs, "budget.unpriced_traffic/budget_monthly_usd/0")
	u := qs[0]
	// The event is about the quality of the figures, not about a limit.
	for _, k := range []string{"threshold", "used", "limit_value", "ratio"} {
		if _, ok := u.body[k]; ok {
			t.Errorf("budget.unpriced_traffic has %q", k)
		}
	}
	subj, _ := u.body["subject"].(map[string]any)
	if subj["kind"] != "project" || subj["id"] != strconv.FormatInt(r.p.ID, 10) || u.body["estimate"] != true ||
		u.num("unpriced_requests") != 1 || u.str("limit") != "budget_monthly_usd" {
		t.Errorf("payload = %v", u.body)
	}
	if u.str("id") != webhook.EventID(r.ep.ID, u.d.DedupeKey) {
		t.Errorf("payload id %q does not match the delivery", u.str("id"))
	}

	// Raising the budget does not make a second one in the same month.
	if _, err := r.st.SetAlertSettings(context.Background(), r.p.ID, true, 9_000_000); err != nil {
		t.Fatal(err)
	}
	r.round()
	if n := len(r.queued()); n != 1 {
		t.Errorf("%d events after a budget change, want 1", n)
	}

	// The next month starts counting again.
	r.now = time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	r.w = limits.Windows(r.now)
	r.round() // no unpriced request yet this month
	if n := len(r.queued()); n != 1 {
		t.Errorf("%d events at the start of the month, want 1", n)
	}
	r.use(10, 10, 0, true)
	r.round()
	qs = r.queued()
	if len(qs) != 2 || qs[1].str("window_start") != "2026-11-01T00:00:00Z" {
		t.Errorf("November: %v", kinds(qs))
	}
}

func TestNoMoneyBudgetNoMoneyEvents(t *testing.T) {
	r := newRig(t, store.Project{}, 0)
	r.use(10, 10, 9_000_000_000, true)
	r.round()
	want(t, r.queued())

	// A budget set later in the month: the notice comes in the first round after.
	if _, err := r.st.SetAlertSettings(context.Background(), r.p.ID, true, 1_000_000); err != nil {
		t.Fatal(err)
	}
	r.round()
	want(t, r.queued(), "limit.threshold/budget_monthly_usd/80", "limit.threshold/budget_monthly_usd/100",
		"budget.unpriced_traffic/budget_monthly_usd/0")
}

func TestNothingForAProjectThatIsOffOrHasNoEndpoint(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, store.Project{BudgetDailyTokens: 100}, 0)
	r.use(100, 0, 0, false)

	if _, err := r.st.SetAlertSettings(ctx, r.p.ID, false, 0); err != nil {
		t.Fatal(err)
	}
	r.round()
	want(t, r.queued())

	if _, err := r.st.SetAlertSettings(ctx, r.p.ID, true, 0); err != nil {
		t.Fatal(err)
	}
	if err := r.st.DisableWebhookEndpoint(ctx, r.ep.ID); err != nil {
		t.Fatal(err)
	}
	r.round()
	want(t, r.queued())

	// The endpoint comes back (a new URL re-opens it): the standing breach is sent now.
	if _, err := r.st.UpdateWebhookEndpointURL(ctx, r.ep.ID, "http://127.0.0.1:9/new"); err != nil {
		t.Fatal(err)
	}
	r.round()
	want(t, r.queued(), "limit.threshold/budget_daily_tokens/80", "limit.threshold/budget_daily_tokens/100")

	// No endpoint at all.
	if err := r.st.DeleteWebhookEndpoint(ctx, r.ep.ID); err != nil {
		t.Fatal(err)
	}
	r.use(100, 0, 0, false)
	r.round()
	var n int
	if err := r.st.DB().QueryRow(ctx, "SELECT count(*) FROM webhook_deliveries").Scan(&n); err != nil || n != 0 {
		t.Errorf("%d deliveries left after the endpoint was deleted, %v", n, err)
	}
}

// Only the replica holding the advisory lock evaluates.
func TestOnlyTheLockHolderEvaluates(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, store.Project{BudgetDailyTokens: 100}, 0)
	r.use(100, 0, 0, false)

	release, ok, err := r.st.TryAdvisoryLock(ctx, store.LockAlerts)
	if err != nil || !ok {
		t.Fatalf("test could not take the lock: %v, %v", ok, err)
	}
	ran, err := r.job.RunOnce(ctx)
	if err != nil || ran {
		t.Fatalf("with the lock held elsewhere: ran=%v err=%v", ran, err)
	}
	want(t, r.queued())

	release()
	r.round()
	want(t, r.queued(), "limit.threshold/budget_daily_tokens/80", "limit.threshold/budget_daily_tokens/100")

	// Two rounds racing for the lock never queue an event twice.
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = r.job.RunOnce(ctx)
		}()
	}
	wg.Wait()
	if n := len(r.queued()); n != 2 {
		t.Errorf("%d events after concurrent rounds, want 2", n)
	}
}

// A money budget far exceeded changes nothing for the request: the limiter
// still admits it. And a receiver that fails costs a retry in the outbox,
// not a failed request.
func TestMoneyOverrunAndFailingReceiverDoNotTouchRequests(t *testing.T) {
	ctx := context.Background()
	var hits int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	r := newRig(t, store.Project{}, 1_000_000)
	if _, err := r.st.UpdateWebhookEndpointURL(ctx, r.ep.ID, srv.URL); err != nil {
		t.Fatal(err)
	}
	lim := &limits.Limiter{Store: r.st, Now: func() time.Time { return r.now }}
	subject := limits.Subject{Project: r.p}
	for i := range 5 {
		d, err := lim.CheckSubject(ctx, subject, 100)
		if err != nil || !d.Allowed {
			t.Fatalf("request %d: %+v, %v", i, d, err)
		}
		// Ten dollars each against a one dollar budget.
		if err := lim.RecordSubject(ctx, subject, limits.Spend{PromptTokens: 100, CompletionTokens: 20,
			CostMicros: 10_000_000, CountRequest: !d.Reserved}); err != nil {
			t.Fatal(err)
		}
	}
	r.round()
	want(t, r.queued(), "limit.threshold/budget_monthly_usd/80", "limit.threshold/budget_monthly_usd/100")

	w := webhook.NewWorker(r.st, webhook.NewClient(map[string]bool{"127.0.0.1": true}), webhook.WithLogger(quiet))
	if n, err := w.RunOnce(ctx); err != nil || n != 2 {
		t.Fatalf("worker claimed %d, %v; want the two alerts", n, err)
	}
	mu.Lock()
	got := hits
	mu.Unlock()
	if got != 2 {
		t.Errorf("receiver saw %d requests, want 2", got)
	}
	for _, q := range r.queued() {
		if q.d.Status != store.DeliveryPending || q.d.Attempts != 1 || q.d.LastStatus == nil || *q.d.LastStatus != 500 {
			t.Errorf("delivery after a 500: %+v", q.d)
		}
	}
	// The request path is as it was.
	d, err := lim.CheckSubject(ctx, subject, 100)
	if err != nil || !d.Allowed {
		t.Fatalf("after the failed delivery: %+v, %v", d, err)
	}
}

// A receiver's reply to a queued alert: the webhook-id header is the payload id.
func TestDeliveredAlertCarriesItsIDInTheHeader(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	var header, sig string
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		mu.Lock()
		header, sig, body = req.Header.Get("webhook-id"), req.Header.Get("webhook-signature"), b
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	r := newRig(t, store.Project{BudgetDailyTokens: 100}, 0)
	if _, err := r.st.UpdateWebhookEndpointURL(ctx, r.ep.ID, srv.URL); err != nil {
		t.Fatal(err)
	}
	r.use(80, 0, 0, false)
	r.round()
	w := webhook.NewWorker(r.st, webhook.NewClient(map[string]bool{"127.0.0.1": true}), webhook.WithLogger(quiet))
	if n, err := w.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("claimed %d, %v", n, err)
	}
	mu.Lock()
	defer mu.Unlock()
	var p map[string]any
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	if header == "" || p["id"] != header || !strings.HasPrefix(sig, "v1,") {
		t.Errorf("webhook-id %q, payload id %v, signature %q", header, p["id"], sig)
	}
	if q := r.queued()[0]; q.d.Status != store.DeliveryDelivered {
		t.Errorf("status = %s", q.d.Status)
	}
}

// The receiver answers 410: the endpoint closes and the trail says so, once,
// without the URL.
func TestGoneReceiverIsAuditedOnce(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusGone) }))
	t.Cleanup(srv.Close)
	r := newRig(t, store.Project{BudgetDailyTokens: 100}, 0)
	if _, err := r.st.UpdateWebhookEndpointURL(ctx, r.ep.ID, srv.URL+"/secret-token"); err != nil {
		t.Fatal(err)
	}
	r.use(80, 0, 0, false)
	r.round()

	w := webhook.NewWorker(alerts.NewAuditedDeliveries(r.st, quiet), webhook.NewClient(map[string]bool{"127.0.0.1": true}),
		webhook.WithLogger(quiet))
	if _, err := w.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	ep, err := r.st.GetWebhookEndpointByID(ctx, r.ep.ID)
	if err != nil || ep.DisabledAt == nil {
		t.Fatalf("endpoint after 410: %+v, %v", ep, err)
	}

	rows := func() []*store.AuditLog {
		t.Helper()
		got, _, err := r.st.ListAuditLogs(ctx, store.AuditFilter{Action: alerts.ActionEndpointDisabled})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	got := rows()
	if len(got) != 1 {
		t.Fatalf("%d audit rows, want 1", len(got))
	}
	a := got[0]
	if a.Action != "webhook.endpoint_disabled" || a.TargetType != "webhook_endpoint" || a.TargetID == nil || *a.TargetID != r.ep.ID ||
		a.ActorUsername != "system" || a.ActorUserID != nil {
		t.Errorf("audit row = %+v", a)
	}
	if a.Details["project_id"] != float64(r.p.ID) || a.Details["purpose"] != "alerts" {
		t.Errorf("details = %v", a.Details)
	}
	if raw, _ := json.Marshal(a.Details); strings.Contains(string(raw), "secret-token") || strings.Contains(string(raw), "127.0.0.1") {
		t.Errorf("the audit entry carries the URL: %s", raw)
	}

	// Closing it again changes nothing, so it is not recorded again.
	if err := alerts.NewAuditedDeliveries(r.st, quiet).DisableWebhookEndpoint(ctx, r.ep.ID); err != nil {
		t.Fatal(err)
	}
	if n := len(rows()); n != 1 {
		t.Errorf("%d audit rows after a second close, want 1", n)
	}
	// An unknown endpoint is the store's ErrNotFound and writes nothing.
	if err := alerts.NewAuditedDeliveries(r.st, quiet).DisableWebhookEndpoint(ctx, r.ep.ID+999); err == nil {
		t.Error("unknown endpoint: no error")
	}
	if n := len(rows()); n != 1 {
		t.Errorf("%d audit rows after an unknown endpoint, want 1", n)
	}
}

// Several deliveries (and replicas) hear the same 410 at once: one audit row,
// written by the call that closed the endpoint.
func TestGoneReceiverHeardByManyDeliveriesIsAuditedOnce(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusGone) }))
	t.Cleanup(srv.Close)
	r := newRig(t, store.Project{BudgetDailyTokens: 100}, 0)
	if _, err := r.st.UpdateWebhookEndpointURL(ctx, r.ep.ID, srv.URL); err != nil {
		t.Fatal(err)
	}
	r.use(100, 0, 0, false) // 80 % and 100 % in one round: two deliveries
	r.round()
	want(t, r.queued(), "limit.threshold/budget_daily_tokens/80", "limit.threshold/budget_daily_tokens/100")

	audits := func() int {
		t.Helper()
		got, _, err := r.st.ListAuditLogs(ctx, store.AuditFilter{Action: alerts.ActionEndpointDisabled})
		if err != nil {
			t.Fatal(err)
		}
		return len(got)
	}
	w := webhook.NewWorker(alerts.NewAuditedDeliveries(r.st, quiet), webhook.NewClient(map[string]bool{"127.0.0.1": true}),
		webhook.WithLogger(quiet), webhook.WithConcurrency(4))
	if n, err := w.RunOnce(ctx); err != nil || n != 2 {
		t.Fatalf("claimed %d, %v; want both deliveries in one batch", n, err)
	}
	if n := audits(); n != 1 {
		t.Errorf("%d audit rows after two deliveries heard the 410, want 1", n)
	}

	// The same race, forced: many callers close the endpoint at the same time.
	if _, err := r.st.UpdateWebhookEndpointURL(ctx, r.ep.ID, srv.URL); err != nil { // open again
		t.Fatal(err)
	}
	wrapped := alerts.NewAuditedDeliveries(r.st, quiet)
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := wrapped.DisableWebhookEndpoint(ctx, r.ep.ID); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := audits(); n != 2 {
		t.Errorf("%d audit rows after 16 concurrent closes of an open endpoint, want 2 in all (one per closing)", n)
	}
}

// A limit crossed in the last minute of a window, while no round ran, is
// reported by the first round after: the closed window is still looked at.
func TestBreachAtTheEndOfAWindowIsNotLostAcrossTheBoundary(t *testing.T) {
	r := newRig(t, store.Project{BudgetDailyTokens: 100, BudgetMonthlyTokens: 1000}, 1_000_000)
	r.now = time.Date(2026, 10, 31, 23, 59, 10, 0, time.UTC)
	r.w = limits.Windows(r.now)
	r.round()
	want(t, r.queued())

	r.now = time.Date(2026, 10, 31, 23, 59, 40, 0, time.UTC)
	r.use(100, 0, 2_000_000, true) // 100 % of the day, 10 % of the month, over the money budget
	r.use(900, 0, 0, false)        // and now the month too

	r.now = time.Date(2026, 11, 1, 0, 0, 10, 0, time.UTC)
	r.round()
	qs := r.queued()
	got := kinds(qs)
	slices.Sort(got)
	wantKinds := []string{
		"budget.unpriced_traffic/budget_monthly_usd/0",
		"limit.threshold/budget_daily_tokens/100", "limit.threshold/budget_daily_tokens/80",
		"limit.threshold/budget_monthly_tokens/100", "limit.threshold/budget_monthly_tokens/80",
		"limit.threshold/budget_monthly_usd/100", "limit.threshold/budget_monthly_usd/80"}
	if !slices.Equal(got, wantKinds) {
		t.Fatalf("queued %v, want %v", got, wantKinds)
	}
	for _, q := range qs {
		daily := q.str("limit") == "budget_daily_tokens"
		ws, we := q.str("window_start"), q.str("window_end")
		if daily && (ws != "2026-10-31T00:00:00Z" || we != "2026-11-01T00:00:00Z") ||
			!daily && (ws != "2026-10-01T00:00:00Z" || we != "2026-11-01T00:00:00Z") {
			t.Errorf("%s window = %s .. %s", q.str("limit"), ws, we)
		}
	}
	// Rounds after that add nothing, in the new window or the old one.
	r.round()
	r.now = r.now.Add(time.Hour)
	r.round()
	if n := len(r.queued()); n != 7 {
		t.Errorf("%d events after more rounds, want 7", n)
	}
}

// The closed window is measured against the limit that was in force while it
// was open. A money budget set, or a token budget raised, after the window
// ended must not make the first round after it report the old window against
// a limit that did not exist then.
func TestLimitSetAfterTheWindowEndedDoesNotWarnForTheClosedWindow(t *testing.T) {
	ctx := context.Background()

	t.Run("money budget set in the new month", func(t *testing.T) {
		r := newRig(t, store.Project{}, 0)
		r.now = time.Date(2026, 10, 31, 23, 59, 40, 0, time.UTC)
		r.w = limits.Windows(r.now)
		r.use(10, 10, 5_000_000, true) // five dollars in October, and unpriced traffic, with no budget
		r.now = time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC)
		if _, err := r.st.SetAlertSettings(ctx, r.p.ID, true, 1_000_000); err != nil {
			t.Fatal(err)
		}
		r.edited(r.now)
		r.round()
		want(t, r.queued())
		// The same breach with the budget in force before the month ended is reported.
		r.edited(time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC))
		r.round()
		want(t, r.queued(), "limit.threshold/budget_monthly_usd/80", "limit.threshold/budget_monthly_usd/100",
			"budget.unpriced_traffic/budget_monthly_usd/0")
	})

	t.Run("daily token budget raised the next morning", func(t *testing.T) {
		r := newRig(t, store.Project{BudgetDailyTokens: 1000}, 0)
		r.now = time.Date(2026, 10, 7, 23, 59, 40, 0, time.UTC)
		r.w = limits.Windows(r.now)
		r.use(1000, 0, 0, false)
		r.round()
		want(t, r.queued(), "limit.threshold/budget_daily_tokens/80", "limit.threshold/budget_daily_tokens/100")

		p := *r.p
		p.BudgetDailyTokens = 1200
		if _, err := r.st.UpdateProject(ctx, &p); err != nil {
			t.Fatal(err)
		}
		r.now = time.Date(2026, 10, 8, 1, 0, 0, 0, time.UTC)
		r.edited(r.now)
		r.round()
		// Yesterday's 1000 of 1200 is 83 %, but the limit was 1000 yesterday.
		want(t, r.queued(), "limit.threshold/budget_daily_tokens/80", "limit.threshold/budget_daily_tokens/100")
	})

	t.Run("end user ceiling raised the next morning", func(t *testing.T) {
		r := newRig(t, store.Project{EndUserDailyTokens: 100}, 0)
		r.now = time.Date(2026, 10, 7, 23, 59, 40, 0, time.UTC)
		r.w = limits.Windows(r.now)
		if err := r.st.RecordEndUserUsage(ctx, r.p.ID, "carol", r.w, 90, 0, true); err != nil {
			t.Fatal(err)
		}
		r.now = time.Date(2026, 10, 8, 1, 0, 0, 0, time.UTC)
		p := *r.p
		p.EndUserDailyTokens = 80 // lowered: carol's 90 now exceeds it, but only the new limit says so
		if _, err := r.st.UpdateProject(ctx, &p); err != nil {
			t.Fatal(err)
		}
		r.edited(r.now)
		r.round()
		want(t, r.queued())
	})

	t.Run("a key budget edited the next morning", func(t *testing.T) {
		r := newRig(t, store.Project{}, 0)
		u, err := r.st.CreateUser(ctx, "owner", "h", "admin")
		if err != nil {
			t.Fatal(err)
		}
		k, _, err := r.st.CreateAPIKey(ctx, &store.APIKey{Kind: store.KindGateway, Name: "k", UserID: u.ID,
			BudgetDailyTokens: 100, ProjectIDs: []int64{r.p.ID}})
		if err != nil {
			t.Fatal(err)
		}
		w := limits.Windows(time.Date(2026, 10, 7, 23, 59, 40, 0, time.UTC))
		if err := r.st.RecordKeyUsage(ctx, k.ID, w, 90, 0, true); err != nil {
			t.Fatal(err)
		}
		set := func(at time.Time) {
			if _, err := r.st.DB().Exec(ctx, "UPDATE api_keys SET updated_at = $2 WHERE id = $1", k.ID, at); err != nil {
				t.Fatal(err)
			}
		}
		r.now = time.Date(2026, 10, 8, 1, 0, 0, 0, time.UTC)
		set(r.now) // edited after the day ended
		r.round()
		want(t, r.queued())
		set(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) // unchanged since: the breach is reported
		r.round()
		want(t, r.queued(), "limit.threshold/budget_daily_tokens/80")
	})
}

// Past LateWindow a closed window is let go: a breach nobody saw is not
// reported hours later against a limit that may have changed since.
func TestClosedWindowIsLetGoAfterTheGracePeriod(t *testing.T) {
	r := newRig(t, store.Project{BudgetDailyTokens: 100, BudgetMonthlyTokens: 1000}, 0)
	r.now = time.Date(2026, 10, 31, 23, 59, 40, 0, time.UTC)
	r.w = limits.Windows(r.now)
	r.use(1000, 0, 0, false)
	r.now = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC).Add(alerts.LateWindow)
	r.round()
	want(t, r.queued())
}

// Rounds that find nothing new open no transaction per event: a limit that
// stays crossed for weeks must not burn the id sequence a thousand times.
func TestKnownEventsCostNoTransaction(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, store.Project{BudgetDailyTokens: 100}, 0)
	r.use(100, 0, 0, false)
	for range 6 {
		r.round()
	}
	want(t, r.queued(), "limit.threshold/budget_daily_tokens/80", "limit.threshold/budget_daily_tokens/100")
	var next int64
	if err := r.st.DB().QueryRow(ctx, "SELECT nextval(pg_get_serial_sequence('alert_events', 'id'))").Scan(&next); err != nil {
		t.Fatal(err)
	}
	if next != 3 {
		t.Errorf("the next alert_events id is %d, want 3: repeated rounds burned ids by inserting known events", next)
	}
}

// An endpoint a 410 closed, or one that was deleted, did not receive what was
// still queued for it. When it is open again (or replaced) a limit that is
// still crossed is sent; what the receiver already got is not sent twice.
func TestUnsentAlertsComeBackWhenTheEndpointIsOpenAgain(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	setStatus := func(s int) { mu.Lock(); status = s; mu.Unlock() }

	r := newRig(t, store.Project{BudgetDailyTokens: 100}, 0)
	if _, err := r.st.UpdateWebhookEndpointURL(ctx, r.ep.ID, srv.URL); err != nil {
		t.Fatal(err)
	}
	worker := webhook.NewWorker(alerts.NewAuditedDeliveries(r.st, quiet), webhook.NewClient(map[string]bool{"127.0.0.1": true}),
		webhook.WithLogger(quiet))

	r.use(80, 0, 0, false) // 80 %: delivered
	r.round()
	if n, err := worker.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("claimed %d, %v", n, err)
	}
	r.use(20, 0, 0, false) // 100 %: queued, then the receiver says 410
	r.round()
	setStatus(http.StatusGone)
	if n, err := worker.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("claimed %d, %v", n, err)
	}
	if ep, err := r.st.GetWebhookEndpointMeta(ctx, r.p.ID, "alerts"); err != nil || ep.DisabledAt == nil {
		t.Fatalf("endpoint = %+v, %v; want it closed", ep, err)
	}
	r.round() // closed: nothing is queued for it
	if n := len(r.queued()); n != 2 {
		t.Fatalf("%d deliveries while closed, want the 2 old ones", n)
	}

	// The operator saves the URL again: only the 100 % alert, which never arrived, is sent.
	setStatus(http.StatusOK)
	if _, err := r.st.SetWebhookEndpointURL(ctx, r.ep.ID, srv.URL); err != nil {
		t.Fatal(err)
	}
	r.round()
	qs := r.queued()
	want(t, qs, "limit.threshold/budget_daily_tokens/80", "limit.threshold/budget_daily_tokens/100",
		"limit.threshold/budget_daily_tokens/100")
	if qs[0].d.Status != store.DeliveryDelivered || qs[1].d.Status != store.DeliveryFailed || qs[2].d.Status != store.DeliveryPending {
		t.Errorf("statuses = %s, %s, %s", qs[0].d.Status, qs[1].d.Status, qs[2].d.Status)
	}

	// Deleting the endpoint does the same for its replacement.
	if err := r.st.DeleteWebhookEndpointForgettingUnsent(ctx, r.ep.ID); err != nil {
		t.Fatal(err)
	}
	fresh, err := r.st.CreateWebhookEndpoint(ctx, &store.WebhookEndpoint{ProjectID: r.p.ID, Purpose: store.WebhookPurposeAlerts,
		URL: srv.URL, Secret: "whsec_new"})
	if err != nil {
		t.Fatal(err)
	}
	r.ep = fresh
	r.round()
	// The 80 % alert was delivered to the old endpoint and stays remembered (the
	// project's receiver was told); the 100 % one was never delivered.
	want(t, r.queued(), "limit.threshold/budget_daily_tokens/100")
	if err := r.st.DeleteWebhookEndpointForgettingUnsent(ctx, 987654); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown endpoint: %v", err)
	}
}

func TestRunStopsWhenTheContextIsCancelled(t *testing.T) {
	r := newRig(t, store.Project{BudgetDailyTokens: 100}, 0)
	job := alerts.NewJob(r.st, alerts.WithLogger(quiet), alerts.WithInterval(10*time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { job.Run(ctx); close(done) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Job.Run did not return after its context was cancelled")
	}
}

func TestWorkerRunStopsWhenTheContextIsCancelled(t *testing.T) {
	r := newRig(t, store.Project{}, 0)
	w := webhook.NewWorker(alerts.NewAuditedDeliveries(r.st, quiet), webhook.NewClient(nil),
		webhook.WithLogger(quiet), webhook.WithPollInterval(10*time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Worker.Run did not return after its context was cancelled")
	}
}

// Compile-time proof that the real store is what the job and the worker take.
var (
	_ alerts.Store      = (*store.Store)(nil)
	_ alerts.AuditStore = (*store.Store)(nil)
)
