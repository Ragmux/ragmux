package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ragmux/ragmux/internal/testdb"
)

// seriesWithPrefix counts the exported series of one metric family, which is
// the number the cardinality rule is actually about.
func seriesWithPrefix(text, prefix string) int {
	n := 0
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, prefix) {
			n++
		}
	}
	return n
}

// TestInventedMethodCollapsesToOther: r.Method is a token the client writes
// on the request line, so a client that invents a verb per request would
// otherwise mint a series per request. Every verb outside the closed set has
// to land in one bucket, and the invented token must not appear anywhere in
// the exposition.
func TestInventedMethodCollapsesToOther(t *testing.T) {
	e := newEnv(t, testdb.Config(t))

	// One known verb first, so the test can tell "collapsed" from "not
	// recorded at all".
	before := seriesWithPrefix(e.registry.Text(), "ragmux_http_requests_total{")

	invented := []string{"PROPFIND", "FROBNICATE", "X-9f2c1b7e4a3d", "CHECKOUT", "BREW"}
	for _, m := range invented {
		req, err := http.NewRequest(m, e.srv.URL+"/healthz", nil)
		if err != nil {
			t.Fatalf("build %s request: %v", m, err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s /healthz: %v", m, err)
		}
		resp.Body.Close()
	}

	text := e.registry.Text()
	if !strings.Contains(text, `method="other"`) {
		t.Errorf("five invented verbs produced no method=\"other\" series:\n%s", httpLines(text))
	}
	for _, m := range invented {
		if strings.Contains(text, m) {
			t.Errorf("the invented method %q reached a label; PRD rule 10 bounds labels by "+
				"the installation, not by traffic:\n%s", m, httpLines(text))
		}
	}

	// Five distinct invented verbs against one route and one status are one
	// new series between them, not five.
	after := seriesWithPrefix(text, "ragmux_http_requests_total{")
	if after-before > 1 {
		t.Errorf("five invented verbs added %d series, want at most 1:\n%s", after-before, httpLines(text))
	}
}

// TestUnknownUpstreamErrorTypeCollapsesToOther: provider.Error.Type is
// copied straight out of the upstream response body, so the upstream chooses
// the label value. An upstream returning a fresh type per response -- an id,
// a sentence, a timestamp -- must not mint a series per response.
func TestUnknownUpstreamErrorTypeCollapsesToOther(t *testing.T) {
	// An upstream that answers every call with a different error type.
	var n int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
			"message": "upstream is unwell",
			"type":    fmt.Sprintf("ERRTYPE-0f3a-b21c-%d", n),
		}})
	}))
	defer up.Close()

	e := newEnv(t, testdb.Config(t))
	conn := e.call("POST", "/admin/api/models", map[string]any{"name": "mock", "provider_type": "custom_openai",
		"base_url": up.URL + "/v1", "api_key": "secret", "model_name": "mock-model"}, "")
	if conn["_status"] != float64(201) {
		t.Fatalf("create connection: %v", conn)
	}
	proj := e.call("POST", "/admin/api/projects", map[string]any{"name": "app",
		"model_connection_id": int64(conn["id"].(float64))}, "")
	if proj["_status"] != float64(201) {
		t.Fatalf("create project: %v", proj)
	}
	apiKey := proj["api_key"].(string)

	const calls = 4
	for i := 0; i < calls; i++ {
		chat := e.call("POST", "/v1/chat/completions", map[string]any{
			"messages": []map[string]string{{"role": "user", "content": "hi"}}}, apiKey)
		if chat["_status"] == float64(200) {
			t.Fatalf("call %d unexpectedly succeeded: %v", i, chat)
		}
	}

	text := e.registry.Text()
	if !strings.Contains(text, `type="other"`) {
		t.Errorf("an unrecognised upstream error type produced no type=\"other\" series:\n%s",
			gatewayLines(text))
	}
	if strings.Contains(text, "ERRTYPE-0f3a-b21c") {
		t.Errorf("an upstream-chosen error type reached a label:\n%s", gatewayLines(text))
	}
	// Four calls, four distinct upstream types, one series.
	if got := seriesWithPrefix(text, "ragmux_gateway_errors_total{"); got != 1 {
		t.Errorf("%d error series after %d calls with %d distinct upstream types, want 1:\n%s",
			got, calls, calls, gatewayLines(text))
	}
}
