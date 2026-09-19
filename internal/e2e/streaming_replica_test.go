package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ragmux/ragmux/internal/provider"
	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/internal/testdb"
)

// slowUpstream is an OpenAI-compatible server that sends its SSE chunks one
// at a time with gap in between, so a test can hold a stream open long
// enough to do something to the replica serving it.
func slowUpstream(gap time.Duration, chunks int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req provider.ChatRequest
		_ = json.Unmarshal(body, &req)
		if !req.Stream {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "x", "model": req.Model,
				"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "done"}, "finish_reason": "stop"}},
				"usage":   map[string]int{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no flusher", http.StatusInternalServerError)
			return
		}
		for i := 0; i < chunks; i++ {
			fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]any{"id": "c", "choices": []any{
				map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": fmt.Sprintf("part-%d ", i)}}}}))
			flusher.Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(gap):
			}
		}
		fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]any{"id": "c", "choices": []any{
			map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}}))
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
}

// openStream starts a streaming completion and returns the response plus a
// reader positioned before the first event.
func openStream(t *testing.T, e *env, apiKey string) (*http.Response, *bufio.Reader) {
	t.Helper()
	req, err := http.NewRequest("POST", e.srv.URL+"/v1/chat/completions", bytes.NewReader(mustJSON(map[string]any{
		"stream": true, "messages": []map[string]string{{"role": "user", "content": "hello"}}})))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	// A dedicated client per stream: the default one pools connections, and
	// this test severs connections on purpose.
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp, bufio.NewReader(resp.Body)
}

// readEvent returns the next non-empty SSE line, or "" when the stream ends
// (cleanly or because the connection went away).
func readEvent(r *bufio.Reader) string {
	for {
		line, err := r.ReadString('\n')
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
		if err != nil {
			return ""
		}
	}
}

// chatStatus runs one non-streaming completion and reports the status.
func chatStatus(e *env, apiKey string) int {
	r := e.call("POST", "/v1/chat/completions", map[string]any{
		"messages": []map[string]string{{"role": "user", "content": "hi"}}}, apiKey)
	return int(r["_status"].(float64))
}

// logStatuses lists the request log status codes of one project, oldest first.
func logStatuses(t *testing.T, st *store.Store, projectID int64) []int {
	t.Helper()
	rows, err := st.DB().Query(context.Background(),
		"SELECT status_code FROM request_logs WHERE project_id = $1 ORDER BY id", projectID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var s int
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

// waitForLogs waits until a project has n request log rows.
func waitForLogs(t *testing.T, st *store.Store, projectID int64, n int) []int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var got []int
	for time.Now().Before(deadline) {
		if got = logStatuses(t, st, projectID); len(got) >= n {
			return got
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("project %d has %v request log rows, want %d", projectID, got, n)
	return nil
}

// newProject creates a model connection (once) and a project on e.
func newProject(t *testing.T, e *env, connID int64, name string, rpm int) (string, int64) {
	t.Helper()
	p := e.call("POST", "/admin/api/projects", map[string]any{
		"name": name, "model_connection_id": connID, "rate_limit_rpm": rpm}, "")
	if p["_status"] != float64(201) {
		t.Fatalf("create project %s: %v", name, p)
	}
	return p["api_key"].(string), int64(p["project"].(map[string]any)["id"].(float64))
}

// TestStreamingAcrossReplicasNeedsNoAffinity runs two complete router
// stacks over one database - the test's version of two replicas behind a
// load balancer - and shows that a streaming completion carries no
// server-side state: the same project key streams from both at once, and
// losing the replica that serves one stream leaves the other untouched.
// That is why Ragmux needs no sticky sessions in front of it.
func TestStreamingAcrossReplicasNeedsNoAffinity(t *testing.T) {
	up := slowUpstream(60*time.Millisecond, 5)
	defer up.Close()
	cfg := testdb.Config(t)
	r1 := newEnv(t, cfg)
	r2 := newEnv(t, cfg)

	conn := r1.call("POST", "/admin/api/models", map[string]any{"name": "mock", "provider_type": "custom_openai",
		"base_url": up.URL + "/v1", "api_key": "secret", "model_name": "mock-model"}, "")
	if conn["_status"] != float64(201) {
		t.Fatalf("create connection: %v", conn)
	}
	connID := int64(conn["id"].(float64))

	// The project was created through replica 1 and is used through both:
	// the key lives in the database, not in the process that issued it.
	apiKey, projID := newProject(t, r1, connID, "stream", 0)

	// Two streams at once, one per replica, on the same key.
	var wg sync.WaitGroup
	bodies := make([]string, 2)
	for i, e := range []*env{r1, r2} {
		wg.Add(1)
		go func(i int, e *env) {
			defer wg.Done()
			resp, br := openStream(t, e, apiKey)
			defer resp.Body.Close()
			if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
				t.Errorf("replica %d content-type %q", i+1, ct)
				return
			}
			// Read the first event before the other stream is done, so the
			// two really do overlap.
			var b strings.Builder
			for {
				line := readEvent(br)
				if line == "" {
					break
				}
				b.WriteString(line)
				if line == "data: [DONE]" {
					break
				}
			}
			bodies[i] = b.String()
		}(i, e)
	}
	wg.Wait()
	for i, b := range bodies {
		if !strings.Contains(b, "part-0") || !strings.HasSuffix(b, "data: [DONE]") {
			t.Fatalf("replica %d stream body: %q", i+1, b)
		}
	}
	// Both replicas wrote their own row into the shared request log.
	if got := waitForLogs(t, r1.store, projID, 2); got[0] != 200 || got[1] != 200 {
		t.Errorf("request log statuses after two streams: %v", got)
	}

	// Now lose replica 1 mid-stream. Its httptest.Server is left running
	// (Close would block on the in-flight stream); severing the client
	// connections is what a killed container looks like from outside.
	deadKey, deadProj := newProject(t, r1, connID, "failover", 0)
	resp, br := openStream(t, r1, deadKey)
	if first := readEvent(br); !strings.Contains(first, "part-0") {
		t.Fatalf("first event before the replica dies: %q", first)
	}
	r1.srv.CloseClientConnections()
	for readEvent(br) != "" { //nolint:revive // drain whatever arrived before the connection died
	}
	resp.Body.Close()

	// Replica 1 recorded the interrupted stream as a client disconnect, and
	// a retry on replica 2 - a different process, no shared stream state -
	// just works.
	if got := waitForLogs(t, r1.store, deadProj, 1); got[0] != 499 {
		t.Errorf("interrupted stream on replica 1 logged %v, want 499", got)
	}
	if st := chatStatus(r2, deadKey); st != 200 {
		t.Fatalf("retry on replica 2 after replica 1 died: %d", st)
	}
	got := waitForLogs(t, r2.store, deadProj, 2)
	if got[0] != 499 || got[1] != 200 {
		t.Errorf("request log statuses for the failover project: %v, want [499 200]", got)
	}
}

// TestRateLimitIsSharedAcrossReplicas shows the other half of why no
// affinity is needed: the per-project rate limit is a row in the database,
// so two requests served by replica 1 already spend the budget replica 2
// enforces.
func TestRateLimitIsSharedAcrossReplicas(t *testing.T) {
	up := slowUpstream(time.Millisecond, 1)
	defer up.Close()
	cfg := testdb.Config(t)
	r1 := newEnv(t, cfg)
	r2 := newEnv(t, cfg)
	conn := r1.call("POST", "/admin/api/models", map[string]any{"name": "mock", "provider_type": "custom_openai",
		"base_url": up.URL + "/v1", "api_key": "secret", "model_name": "mock-model"}, "")
	connID := int64(conn["id"].(float64))

	// The limit window is the UTC minute. A sequence that straddles a
	// boundary proves nothing, so it is retried on a fresh project.
	for attempt := 0; attempt < 4; attempt++ {
		apiKey, _ := newProject(t, r1, connID, fmt.Sprintf("limited-%d", attempt), 2)
		minute := time.Now().UTC().Minute()
		first, second := chatStatus(r1, apiKey), chatStatus(r1, apiKey)
		third := chatStatus(r2, apiKey)
		if time.Now().UTC().Minute() != minute {
			continue // the window rolled mid-sequence; start over
		}
		if first != 200 || second != 200 {
			t.Fatalf("first two requests on replica 1: %d %d", first, second)
		}
		if third != 429 {
			t.Fatalf("third request on replica 2 = %d, want 429: the RPM budget is shared", third)
		}
		return
	}
	t.Skip("the UTC minute rolled on every attempt")
}
