package e2e

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/internal/testdb"
)

// chatWithRequestID posts one chat completion, optionally with an
// X-Request-Id header, and returns the status and the response header.
func (e *env) chatWithRequestID(apiKey string, headerValues ...string) (int, string) {
	e.t.Helper()
	return e.chatAsModel(apiKey, "client-asked-model", headerValues...)
}

// chatAsModel is chatWithRequestID with the model the client asks for chosen
// by the caller; an empty model leaves the field out of the body.
func (e *env) chatAsModel(apiKey, model string, headerValues ...string) (int, string) {
	e.t.Helper()
	payload := map[string]any{
		"messages": []map[string]string{{"role": "user", "content": "What color is a banana?"}}}
	if model != "" {
		payload["model"] = model
	}
	body := mustJSON(payload)
	req, _ := http.NewRequest("POST", e.srv.URL+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	for _, v := range headerValues {
		req.Header.Add("X-Request-Id", v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, resp.Header.Get("X-Request-Id")
}

// loggedRequest reads the request_logs row stamped with id. The row is
// written after the response is sent, so it polls briefly.
func (e *env) loggedRequest(id string) (endpoint, requestedModel string, attempt int, found bool) {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var ep, rm *string
		var at *int
		err := e.store.DB().QueryRow(context.Background(),
			`SELECT endpoint, requested_model, attempt FROM request_logs WHERE request_id = $1`, id).Scan(&ep, &rm, &at)
		if err == nil {
			if ep != nil {
				endpoint = *ep
			}
			if rm != nil {
				requestedModel = *rm
			}
			if at != nil {
				attempt = *at
			}
			return endpoint, requestedModel, attempt, true
		}
		if time.Now().After(deadline) {
			return "", "", 0, false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestRequestID covers the three ways a request id comes to be: the client
// sends a usable one, sends none, or sends one the gateway will not use.
func TestRequestID(t *testing.T) {
	up := mockUpstream(t)
	defer up.Close()
	e := newEnv(t, testdb.Config(t))
	_, apiKey, _ := e.stack(t, up.URL, "fruits.md", "# Banana\n\nBananas are yellow and soft.")

	t.Run("client id is echoed and logged", func(t *testing.T) {
		const want = "client-trace_01.A:b"
		status, got := e.chatWithRequestID(apiKey, want)
		if status != http.StatusOK {
			t.Fatalf("status = %d", status)
		}
		if got != want {
			t.Fatalf("X-Request-Id = %q, want %q", got, want)
		}
		ep, rm, attempt, found := e.loggedRequest(want)
		if !found {
			t.Fatalf("no request_logs row with request_id %q", want)
		}
		if ep != "chat.completions" || rm != "client-asked-model" || attempt != 1 {
			t.Errorf("row = endpoint %q requested_model %q attempt %d", ep, rm, attempt)
		}
	})

	t.Run("generated id matches the log", func(t *testing.T) {
		status, got := e.chatWithRequestID(apiKey)
		if status != http.StatusOK {
			t.Fatalf("status = %d", status)
		}
		if got == "" {
			t.Fatal("no X-Request-Id on the response")
		}
		if _, _, _, found := e.loggedRequest(got); !found {
			t.Fatalf("no request_logs row with generated request_id %q", got)
		}
	})

	t.Run("invalid id is replaced, not rejected", func(t *testing.T) {
		for name, bad := range map[string]string{
			"space":      "has space",
			"quote":      `a"b`,
			"non ascii":  "café",
			"too long":   strings.Repeat("a", 129),
			"credential": "sk-live/abc=def+ghi",
		} {
			status, got := e.chatWithRequestID(apiKey, bad)
			if status != http.StatusOK {
				t.Errorf("%s: status = %d, want 200", name, status)
				continue
			}
			if got == "" || got == bad {
				t.Errorf("%s: X-Request-Id = %q, want a generated value", name, got)
				continue
			}
			if _, _, _, found := e.loggedRequest(got); !found {
				t.Errorf("%s: no request_logs row with request_id %q", name, got)
			}
		}
	})

	t.Run("empty model is stored as NULL", func(t *testing.T) {
		const want = "no-model-1"
		if status, _ := e.chatAsModel(apiKey, "", want); status != http.StatusOK {
			t.Fatalf("status = %d", status)
		}
		var rm *string
		e.loggedRequest(want)
		if err := e.store.DB().QueryRow(context.Background(),
			`SELECT requested_model FROM request_logs WHERE request_id = $1`, want).Scan(&rm); err != nil {
			t.Fatal(err)
		}
		if rm != nil {
			t.Errorf("requested_model = %q, want NULL", *rm)
		}
	})

	t.Run("oversized model is cut", func(t *testing.T) {
		const want = "long-model-1"
		if status, _ := e.chatAsModel(apiKey, strings.Repeat("m", 5000), want); status != http.StatusOK {
			t.Fatalf("status = %d", status)
		}
		_, rm, _, found := e.loggedRequest(want)
		if !found {
			t.Fatalf("no request_logs row with request_id %q", want)
		}
		if len(rm) != store.MaxRequestedModelBytes {
			t.Errorf("requested_model has %d bytes, want %d", len(rm), store.MaxRequestedModelBytes)
		}
	})

	t.Run("rate-limited call is logged with its identity", func(t *testing.T) {
		conn := e.call("POST", "/admin/api/models", map[string]any{"name": "limited", "provider_type": "custom_openai",
			"base_url": up.URL + "/v1", "api_key": "secret", "model_name": "mock-model"}, "")
		if conn["_status"] != float64(201) {
			t.Fatalf("create connection: %v", conn)
		}
		pr := e.call("POST", "/admin/api/projects", map[string]any{"name": "limited",
			"model_connection_id": int64(conn["id"].(float64)), "rate_limit_rpm": 1}, "")
		if pr["_status"] != float64(201) {
			t.Fatalf("create project: %v", pr)
		}
		limitedKey := pr["api_key"].(string)
		if status, _ := e.chatWithRequestID(limitedKey, "within-limit-1"); status != http.StatusOK {
			t.Fatalf("first call status = %d", status)
		}
		const want = "over-limit-1"
		status, got := e.chatWithRequestID(limitedKey, want)
		if status != http.StatusTooManyRequests || got != want {
			t.Fatalf("second call: status %d, X-Request-Id %q", status, got)
		}
		ep, rm, attempt, found := e.loggedRequest(want)
		if !found {
			t.Fatalf("no request_logs row for the 429 with request_id %q", want)
		}
		if ep != "chat.completions" || rm != "client-asked-model" {
			t.Errorf("429 row = endpoint %q requested_model %q", ep, rm)
		}
		if attempt != 0 {
			t.Errorf("429 row attempt = %d, want NULL (nothing went upstream)", attempt)
		}
	})

	t.Run("error responses carry the id", func(t *testing.T) {
		const want = "rejected-1"
		status, got := e.chatWithRequestID("not-a-real-key", want)
		if status != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", status)
		}
		if got != want {
			t.Errorf("X-Request-Id on a 401 = %q, want %q", got, want)
		}
		status, got = e.chatWithRequestID("not-a-real-key")
		if status != http.StatusUnauthorized || got == "" {
			t.Errorf("401 without a client id: status %d, X-Request-Id %q", status, got)
		}
	})
}
