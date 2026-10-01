package obs

import (
	"strings"
	"testing"
	"time"

	"github.com/ragmux/ragmux/internal/metrics"
)

// TestUnpricedCountsOnlySuccessfulCompletions: a failed completion has no
// spend to price, so only a 2xx with no matching price row is counted.
func TestUnpricedCountsOnlySuccessfulCompletions(t *testing.T) {
	reg := metrics.New(metrics.Options{})
	m := New(reg)

	m.RecordGateway(GatewayRequest{Project: "1", Model: "m", Provider: "openai", Status: 200, Unpriced: true})
	m.RecordGateway(GatewayRequest{Project: "1", Model: "m", Provider: "openai", Status: 200, Unpriced: true})
	m.RecordGateway(GatewayRequest{Project: "1", Model: "m", Provider: "openai", Status: 200})
	m.RecordGateway(GatewayRequest{Project: "1", Model: "m", Provider: "anthropic", Status: 502,
		ErrorType: "upstream_error", Unpriced: true})

	got := reg.Text()
	if !strings.Contains(got, "# TYPE ragmux_requests_unpriced_total counter") {
		t.Fatalf("unpriced metric missing or not a counter:\n%s", got)
	}
	if !strings.Contains(got, `ragmux_requests_unpriced_total{provider="openai"} 2`) {
		t.Errorf("want two unpriced openai completions:\n%s", got)
	}
	if strings.Contains(got, `ragmux_requests_unpriced_total{provider="anthropic"}`) {
		t.Errorf("a failed completion was counted as unpriced:\n%s", got)
	}
}

// TestRetentionTimestampStartsAtZero: the series exists from start-up, so an
// alert on time() - max(...) fires for a replica set that has never finished
// a pass instead of seeing no data.
func TestRetentionTimestampStartsAtZero(t *testing.T) {
	reg := metrics.New(metrics.Options{})
	m := New(reg)

	got := reg.Text()
	if !strings.Contains(got, "# TYPE ragmux_retention_last_success_timestamp_seconds gauge") {
		t.Fatalf("retention timestamp missing or not a gauge:\n%s", got)
	}
	if !strings.Contains(got, "ragmux_retention_last_success_timestamp_seconds 0\n") {
		t.Fatalf("retention timestamp should read 0 before any pass:\n%s", got)
	}

	m.RetentionPassed(time.Unix(1_800_000_000, 0))
	if got := reg.Text(); !strings.Contains(got, "ragmux_retention_last_success_timestamp_seconds 1.8e+09\n") &&
		!strings.Contains(got, "ragmux_retention_last_success_timestamp_seconds 1800000000\n") {
		t.Fatalf("retention timestamp not set:\n%s", got)
	}

	var nilMetrics *Metrics
	nilMetrics.RetentionPassed(time.Now()) // must not panic
}
