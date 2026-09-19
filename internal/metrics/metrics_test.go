package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestExpositionShape(t *testing.T) {
	r := New(Options{})
	c := r.Counter("ragmux_http_requests_total", "HTTP requests handled.", "route", "status")
	c.With("/v1/chat/completions", "200").Add(42)
	c.With("/healthz", "200").Inc()

	got := r.Text()
	want := `# HELP ragmux_http_requests_total HTTP requests handled.
# TYPE ragmux_http_requests_total counter
ragmux_http_requests_total{route="/v1/chat/completions",status="200"} 42
ragmux_http_requests_total{route="/healthz",status="200"} 1
`
	if !strings.HasPrefix(got, want) {
		t.Fatalf("exposition mismatch:\ngot:\n%s\nwant prefix:\n%s", got, want)
	}
}

func TestRegistrationOrderIsOutputOrder(t *testing.T) {
	r := New(Options{})
	r.Counter("ragmux_b_total", "b").With()
	r.Counter("ragmux_a_total", "a").With()
	got := r.Text()
	if strings.Index(got, "ragmux_b_total") > strings.Index(got, "ragmux_a_total") {
		t.Fatal("families are not emitted in registration order")
	}
}

func TestLabelValueEscaping(t *testing.T) {
	r := New(Options{})
	g := r.Gauge("ragmux_escape_test", "Escaping.", "v")
	g.With(`a"b\c` + "\n" + "d").Set(1)
	got := r.Text()
	if !strings.Contains(got, `ragmux_escape_test{v="a\"b\\c\nd"} 1`) {
		t.Fatalf("label value not escaped: %s", got)
	}
}

func TestHelpEscaping(t *testing.T) {
	r := New(Options{})
	r.Counter("ragmux_help_test_total", "line one\nline two \\ end").With().Inc()
	got := r.Text()
	if !strings.Contains(got, `# HELP ragmux_help_test_total line one\nline two \\ end`) {
		t.Fatalf("help not escaped: %s", got)
	}
}

func TestHistogramIsCumulative(t *testing.T) {
	r := New(Options{})
	h := r.Histogram("ragmux_dur_seconds", "Durations.", []float64{0.1, 0.5, 1}, "route")
	s := h.With("/v1")
	for _, v := range []float64{0.05, 0.2, 0.2, 0.7, 5} {
		s.Observe(v)
	}
	got := r.Text()
	for _, want := range []string{
		`ragmux_dur_seconds_bucket{route="/v1",le="0.1"} 1`,
		`ragmux_dur_seconds_bucket{route="/v1",le="0.5"} 3`,
		`ragmux_dur_seconds_bucket{route="/v1",le="1"} 4`,
		`ragmux_dur_seconds_bucket{route="/v1",le="+Inf"} 5`,
		`ragmux_dur_seconds_sum{route="/v1"} 6.15`,
		`ragmux_dur_seconds_count{route="/v1"} 5`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestHistogramBoundIsInclusive(t *testing.T) {
	r := New(Options{})
	h := r.Histogram("ragmux_edge_seconds", "Edge.", []float64{0.1})
	h.With().Observe(0.1)
	if !strings.Contains(r.Text(), `ragmux_edge_seconds_bucket{le="0.1"} 1`) {
		t.Fatal("a value equal to the bound belongs in that bucket")
	}
}

func TestFloatCounterIgnoresNonPositive(t *testing.T) {
	r := New(Options{})
	c := r.FloatCounter("ragmux_cost_usd_total", "Cost.", "project")
	s := c.With("1")
	s.Add(0.5)
	s.Add(-1)
	if got := s.Value(); got != 0.5 {
		t.Fatalf("counter moved backwards: got %v, want 0.5", got)
	}
}

func TestMaxSeriesDropsAndCounts(t *testing.T) {
	var dropped string
	r := New(Options{MaxSeries: 2})
	r.OnSeriesDropped = func(m string) { dropped = m }
	c := r.Counter("ragmux_capped_total", "Capped.", "id")
	c.With("a").Inc()
	c.With("b").Inc()
	c.With("c").Inc() // refused
	c.With("c").Inc() // still refused, still counted

	if r.Dropped() != 2 {
		t.Errorf("dropped = %d, want 2", r.Dropped())
	}
	if dropped != "ragmux_capped_total" {
		t.Errorf("OnSeriesDropped got %q", dropped)
	}
	got := r.Text()
	if strings.Contains(got, `id="c"`) {
		t.Error("a refused series was exported")
	}
	if !strings.Contains(got, "ragmux_metrics_series_dropped_total 2") {
		t.Errorf("drop counter not exported:\n%s", got)
	}
}

func TestRefusedSeriesStillAcceptsWrites(t *testing.T) {
	// A caller must never have to nil-check the series it just resolved.
	r := New(Options{MaxSeries: 1})
	c := r.Counter("ragmux_overflow_total", "Overflow.", "id")
	c.With("a").Inc()
	c.With("b").Add(5) // handed the shared overflow series; must not panic
}

func TestDuplicateRegistrationPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("registering the same name twice must panic")
		}
	}()
	r := New(Options{})
	r.Counter("ragmux_dup_total", "One.")
	r.Counter("ragmux_dup_total", "Two.")
}

func TestInvalidNamePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("an invalid metric name must panic")
		}
	}()
	New(Options{}).Counter("ragmux-bad-name", "Bad.")
}

func TestLabelArityMismatchPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("a wrong label count must panic")
		}
	}()
	c := New(Options{}).Counter("ragmux_arity_total", "Arity.", "a", "b")
	c.With("only-one")
}

func TestHistogramRejectsUnsortedBounds(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("unsorted bounds must panic")
		}
	}()
	New(Options{}).Histogram("ragmux_unsorted_seconds", "Unsorted.", []float64{1, 0.5})
}

func TestConcurrentIncIsRaceFree(t *testing.T) {
	r := New(Options{MaxSeries: 1000})
	c := r.Counter("ragmux_race_total", "Race.", "id")
	h := r.Histogram("ragmux_race_seconds", "Race.", []float64{0.1, 1}, "id")
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				c.With("shared").Inc()
				h.With("shared").Observe(0.5)
			}
		}(i)
	}
	wg.Wait()
	if got := c.With("shared").Value(); got != 10000 {
		t.Fatalf("counter = %d, want 10000", got)
	}
}

func TestCachedGaugeCallsOncePerTTL(t *testing.T) {
	var calls int
	r := New(Options{})
	r.CachedGaugeFunc("ragmux_documents", "Documents by status.", "status", time.Hour,
		func() (map[string]float64, error) {
			calls++
			return map[string]float64{"ready": 3, "pending": 1}, nil
		})
	_ = r.Text()
	_ = r.Text()
	_ = r.Text()
	if calls != 1 {
		t.Fatalf("value function called %d times, want 1", calls)
	}
	got := r.Text()
	if !strings.Contains(got, `ragmux_documents{status="pending"} 1`) ||
		!strings.Contains(got, `ragmux_documents{status="ready"} 3`) {
		t.Fatalf("cached gauge not exported:\n%s", got)
	}
}

func TestCachedGaugeServesStaleOnError(t *testing.T) {
	var fail bool
	r := New(Options{})
	r.CachedGaugeFunc("ragmux_stale", "Stale.", "status", 0,
		func() (map[string]float64, error) {
			if fail {
				return nil, errFake
			}
			return map[string]float64{"ready": 7}, nil
		})
	_ = r.Text()
	fail = true
	// A scrape that briefly cannot reach the database must read stale rather
	// than report zero, which an alert would read as "the backlog drained".
	if !strings.Contains(r.Text(), `ragmux_stale{status="ready"} 7`) {
		t.Fatal("a failed refresh dropped the previous values")
	}
}

func TestHandlerRequiresToken(t *testing.T) {
	r := New(Options{})
	r.Counter("ragmux_secret_total", "Secret.").With().Inc()
	h := r.Handler("s3cret")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no token: status %d, want 401", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "ragmux_secret_total") {
		t.Error("an unauthorised response leaked the metric set")
	}

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong token: status %d, want 401", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("right token: status %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != contentType {
		t.Errorf("content type = %q, want %q", ct, contentType)
	}
	if !strings.Contains(rec.Body.String(), "ragmux_secret_total 1") {
		t.Error("authorised response is missing the metric")
	}
}

func TestHandlerWithoutTokenIsOpen(t *testing.T) {
	r := New(Options{})
	rec := httptest.NewRecorder()
	r.Handler("").ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
}

func TestRegisterRuntime(t *testing.T) {
	r := New(Options{})
	r.RegisterRuntime("0.4.0", "go1.27.1")
	got := r.Text()
	for _, want := range []string{
		`ragmux_build_info{version="0.4.0",go_version="go1.27.1"} 1`,
		"ragmux_go_goroutines ",
		"ragmux_process_start_time_seconds ",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q", want)
		}
	}
}

var errFake = fakeErr{}

type fakeErr struct{}

func (fakeErr) Error() string { return "boom" }
