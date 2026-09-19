package gateway

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/ragmux/ragmux/internal/limits"
	"github.com/ragmux/ragmux/internal/metrics"
	"github.com/ragmux/ragmux/internal/obs"
	"github.com/ragmux/ragmux/internal/store"
)

// withMetrics gives the gateway a fresh registry and returns it.
func (e *env) withMetrics() *metrics.Registry {
	e.t.Helper()
	reg := metrics.New(metrics.Options{})
	e.gw.Metrics = obs.New(reg)
	return reg
}

func mustContain(t *testing.T, text string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(text, w) {
			t.Errorf("missing %s in:\n%s", w, text)
		}
	}
}

func TestGatewayCountersMove(t *testing.T) {
	e := newEnv(t, nil)
	reg := e.withMetrics()
	project := strconv.FormatInt(e.proj.ID, 10)

	if resp, _ := e.chat(map[string]any{"messages": []map[string]string{{"role": "user", "content": "hi"}}}); resp.StatusCode != http.StatusOK {
		t.Fatalf("chat status %d", resp.StatusCode)
	}
	text := reg.Text()
	mustContain(t, text,
		fmt.Sprintf(`ragmux_gateway_requests_total{project="%s",model="mock-model",status="200",streamed="false"} 1`, project),
		fmt.Sprintf(`ragmux_gateway_tokens_total{project="%s",model="mock-model",kind="prompt"} 5`, project),
		fmt.Sprintf(`ragmux_gateway_tokens_total{project="%s",model="mock-model",kind="completion"} 1`, project),
		`ragmux_gateway_upstream_duration_seconds_count{provider="custom_openai",model="mock-model"} 1`,
	)
	// The provider reported usage, so nothing is estimated and no error
	// series exists at all.
	if strings.Contains(text, "ragmux_gateway_tokens_estimated_total{") {
		t.Errorf("reported counts must not appear as estimated:\n%s", text)
	}
	if strings.Contains(text, "ragmux_gateway_errors_total{") {
		t.Errorf("a successful request produced an error series:\n%s", text)
	}
}

func TestGatewayStreamCountersMove(t *testing.T) {
	e := newEnv(t, nil)
	reg := e.withMetrics()
	project := strconv.FormatInt(e.proj.ID, 10)

	resp := e.post(context.Background(), "/v1/chat/completions",
		map[string]any{"stream": true, "messages": []map[string]string{{"role": "user", "content": "hi"}}}, e.key)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status %d", resp.StatusCode)
	}
	readSSE(t, resp.Body)
	resp.Body.Close()
	text := reg.Text()
	mustContain(t, text,
		fmt.Sprintf(`ragmux_gateway_requests_total{project="%s",model="mock-model",status="200",streamed="true"} 1`, project),
		`ragmux_gateway_stream_chunks_total{provider="custom_openai"} 3`,
		`ragmux_gateway_time_to_first_token_seconds_count{provider="custom_openai",model="mock-model"} 1`,
		// The gauge is back to zero: a stream that finished is not active.
		"ragmux_gateway_streams_active 0",
	)
}

// TestGatewayErrorTypeIsBounded: the error label is provider.Error.Type, a
// closed set. The message, which quotes the upstream body, must not be it.
func TestGatewayErrorTypeIsBounded(t *testing.T) {
	e := newEnv(t, nil)
	reg := e.withMetrics()
	e.up.setChat(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"slow down, request 0f3a-b21c was throttled","type":"rate_limit_error"}}`))
	})
	resp, _ := e.chat(map[string]any{"messages": []map[string]string{{"role": "user", "content": "hi"}}})
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status %d", resp.StatusCode)
	}
	text := reg.Text()
	mustContain(t, text, fmt.Sprintf(
		`ragmux_gateway_errors_total{project="%d",provider="custom_openai",type="rate_limit_error"} 1`, e.proj.ID))
	if strings.Contains(text, "0f3a-b21c") {
		t.Errorf("the upstream message reached a label:\n%s", text)
	}
}

// TestGatewayEstimatedTokensAreSeparate: a provider that reports no usage
// must not silently fill the same series as one that does, or cost maths
// over it is wrong without anything looking wrong.
func TestGatewayEstimatedTokensAreSeparate(t *testing.T) {
	e := newEnv(t, nil)
	reg := e.withMetrics()
	e.up.setChat(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}]}`))
	})
	if resp, _ := e.chat(map[string]any{"messages": []map[string]string{{"role": "user", "content": "hi"}}}); resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	mustContain(t, reg.Text(), fmt.Sprintf(
		`ragmux_gateway_tokens_estimated_total{project="%d",model="mock-model"} 1`, e.proj.ID))
}

// TestLimitDeniedCounterMoves: the reason label is a limits.Reason*
// constant, which is the only reason value that ever reaches it.
func TestLimitDeniedCounterMoves(t *testing.T) {
	e := newEnv(t, func(p *store.Project) { p.RateLimitRPM = 1 })
	reg := e.withMetrics()
	body := map[string]any{"messages": []map[string]string{{"role": "user", "content": "hi"}}}
	if resp, _ := e.chat(body); resp.StatusCode != http.StatusOK {
		t.Fatalf("first chat status %d", resp.StatusCode)
	}
	resp, _ := e.chat(body)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second chat status %d, want 429", resp.StatusCode)
	}
	mustContain(t, reg.Text(), fmt.Sprintf(
		`ragmux_limits_denied_total{project="%d",reason="%s"} 1`, e.proj.ID, limits.ReasonRPM))
}
