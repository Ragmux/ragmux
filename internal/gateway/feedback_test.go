package gateway

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ragmux/ragmux/internal/store"
)

// POST /v1/feedback (ADR-042): a score for a request the project made, found
// by its request_id, taking no content and spending no limit.

// logged writes a request log row for project with the given request_id, the
// row a finished request leaves behind.
func (e *env) logged(projectID int64, requestID string) {
	e.t.Helper()
	if err := e.st.InsertRequestLog(context.Background(), &store.RequestLog{ProjectID: projectID, ModelName: "m",
		StatusCode: 200, RequestID: requestID}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) rate(key string, body any) (*http.Response, map[string]any) {
	e.t.Helper()
	return e.call(http.MethodPost, "/v1/feedback", body, key, nil)
}

// feedbackRows returns what request_feedback holds for a project.
func (e *env) feedbackRows(projectID int64) map[string]struct {
	Score int
	Label *string
	Key   *int64
} {
	e.t.Helper()
	rows, err := e.st.DB().Query(context.Background(),
		"SELECT request_id, score, label, api_key_id FROM request_feedback WHERE project_id = $1", projectID)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]struct {
		Score int
		Label *string
		Key   *int64
	}{}
	for rows.Next() {
		var id string
		var r struct {
			Score int
			Label *string
			Key   *int64
		}
		var score int16
		if err := rows.Scan(&id, &score, &r.Label, &r.Key); err != nil {
			e.t.Fatal(err)
		}
		r.Score = int(score)
		out[id] = r
	}
	return out
}

func TestFeedbackRecordsScoreAndLabel(t *testing.T) {
	e := newEnv(t, nil)
	e.logged(e.proj.ID, "req-1")
	e.logged(e.proj.ID, "req-2")

	resp, out := e.rate(e.key, map[string]any{"request_id": "req-1", "score": 1, "label": "helpful"})
	if resp.StatusCode != http.StatusOK || out["request_id"] != "req-1" || out["score"] != float64(1) || out["label"] != "helpful" {
		t.Fatalf("with label: %d %v", resp.StatusCode, out)
	}
	resp, out = e.rate(e.key, map[string]any{"request_id": "req-2", "score": -1})
	if resp.StatusCode != http.StatusOK || out["label"] != nil {
		t.Fatalf("without label: %d %v", resp.StatusCode, out)
	}
	rows := e.feedbackRows(e.proj.ID)
	if len(rows) != 2 || rows["req-1"].Score != 1 || rows["req-1"].Label == nil || *rows["req-1"].Label != "helpful" ||
		rows["req-2"].Score != -1 || rows["req-2"].Label != nil {
		t.Errorf("rows = %+v", rows)
	}
	// The project's own key is not a user key: no key id to attribute.
	if rows["req-1"].Key != nil {
		t.Errorf("project key attributed to api key %d", *rows["req-1"].Key)
	}

	// A user key is attributed.
	k, raw := e.userKey("front-end", []int64{e.proj.ID}, nil)
	if resp, out := e.rate(raw, map[string]any{"request_id": "req-2", "score": 1}); resp.StatusCode != http.StatusOK {
		t.Fatalf("user key: %d %v", resp.StatusCode, out)
	}
	if got := e.feedbackRows(e.proj.ID)["req-2"]; got.Key == nil || *got.Key != k.ID {
		t.Errorf("req-2 key = %v, want %d", got.Key, k.ID)
	}
}

// A request just answered through the gateway is rated by the id the response
// carried in X-Request-Id; the row is written after the response, so the call
// waits for it the way the documentation tells a client to.
func TestFeedbackForARealRequest(t *testing.T) {
	e := newEnv(t, nil)
	resp, _ := e.chat(map[string]any{"messages": userMsg})
	id := resp.Header.Get("X-Request-Id")
	if id == "" {
		t.Fatal("no X-Request-Id")
	}
	var last *http.Response
	var out map[string]any
	for i := 0; i < 50; i++ {
		last, out = e.rate(e.key, map[string]any{"request_id": id, "score": 1})
		if last.StatusCode != http.StatusNotFound {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if last.StatusCode != http.StatusOK {
		t.Fatalf("%d %v", last.StatusCode, out)
	}
	// A client-chosen id works the same way.
	resp2, _ := e.call(http.MethodPost, "/v1/chat/completions", map[string]any{"messages": userMsg}, e.key,
		map[string]string{"X-Request-Id": "client-chosen.1"})
	if resp2.StatusCode != 200 {
		t.Fatalf("chat: %d", resp2.StatusCode)
	}
	e.lastLog()
	for i := 0; i < 50; i++ {
		last, out = e.rate(e.key, map[string]any{"request_id": "client-chosen.1", "score": -1})
		if last.StatusCode != http.StatusNotFound {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if last.StatusCode != http.StatusOK {
		t.Fatalf("client id: %d %v", last.StatusCode, out)
	}
}

func TestFeedbackValidation(t *testing.T) {
	e := newEnv(t, nil)
	e.logged(e.proj.ID, "req-1")
	long := strings.Repeat("a", 129)
	for name, body := range map[string]any{
		"score 0":             map[string]any{"request_id": "req-1", "score": 0},
		"score 2":             map[string]any{"request_id": "req-1", "score": 2},
		"score -2":            map[string]any{"request_id": "req-1", "score": -2},
		"score float":         `{"request_id": "req-1", "score": 1.5}`,
		"score string":        map[string]any{"request_id": "req-1", "score": "1"},
		"score missing":       map[string]any{"request_id": "req-1"},
		"score null":          `{"request_id": "req-1", "score": null}`,
		"request_id missing":  map[string]any{"score": 1},
		"request_id empty":    map[string]any{"request_id": "", "score": 1},
		"request_id space":    map[string]any{"request_id": "req 1", "score": 1},
		"request_id too long": map[string]any{"request_id": long, "score": 1},
		"request_id number":   map[string]any{"request_id": 5, "score": 1},
		"comment":             map[string]any{"request_id": "req-1", "score": 1, "comment": "great"},
		"unknown field":       map[string]any{"request_id": "req-1", "score": 1, "extra": true},
		"label upper":         map[string]any{"request_id": "req-1", "score": 1, "label": "Helpful"},
		"label empty":         map[string]any{"request_id": "req-1", "score": 1, "label": ""},
		"label space":         map[string]any{"request_id": "req-1", "score": 1, "label": "very good"},
		"label too long":      map[string]any{"request_id": "req-1", "score": 1, "label": strings.Repeat("a", 65)},
		"label number":        map[string]any{"request_id": "req-1", "score": 1, "label": 3},
		"not an object":       `[1]`,
		"two objects":         `{"request_id": "req-1", "score": 1}{"request_id": "req-1", "score": -1}`,
		"empty body":          ``,
	} {
		resp := e.post(context.Background(), "/v1/feedback", body, e.key)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, resp.StatusCode)
		}
	}
	if rows := e.feedbackRows(e.proj.ID); len(rows) != 0 {
		t.Errorf("a refused body left rows: %+v", rows)
	}
	// The error says what is wrong, in the client API's shape.
	resp, out := e.rate(e.key, map[string]any{"request_id": "req-1", "score": 1, "comment": "x"})
	if resp.StatusCode != 400 || errorField(t, out, "type") != "invalid_request_error" || errorField(t, out, "code") != "invalid_feedback" {
		t.Errorf("comment: %d %v", resp.StatusCode, out)
	}
	// The boundary values pass: a 128 character id and a 64 character label.
	e.logged(e.proj.ID, strings.Repeat("a", 128))
	resp, out = e.rate(e.key, map[string]any{"request_id": strings.Repeat("a", 128), "score": 1, "label": strings.Repeat("a", 64)})
	if resp.StatusCode != 200 {
		t.Errorf("boundary: %d %v", resp.StatusCode, out)
	}
	// A body over the bound is refused before it is parsed.
	resp = e.post(context.Background(), "/v1/feedback", `{"request_id": "req-1", "score": 1, "label": "`+strings.Repeat("a", 5000)+`"}`, e.key)
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("large body: %d, want 413", resp.StatusCode)
	}
}

func TestFeedbackUnknownAndForeignIDsAnswerAlike(t *testing.T) {
	e := newEnv(t, nil)
	other := e.newProject("other", nil)
	e.logged(other.ID, "theirs")

	respUnknown, unknown := e.rate(e.key, map[string]any{"request_id": "never-seen", "score": 1})
	respForeign, foreign := e.rate(e.key, map[string]any{"request_id": "theirs", "score": 1})
	for name, c := range map[string]struct {
		resp *http.Response
		out  map[string]any
	}{"unknown": {respUnknown, unknown}, "another project's": {respForeign, foreign}} {
		if c.resp.StatusCode != http.StatusNotFound || errorField(t, c.out, "type") != "not_found_error" ||
			errorField(t, c.out, "code") != "request_not_found" {
			t.Errorf("%s: %d %v", name, c.resp.StatusCode, c.out)
		}
	}
	if errorField(t, unknown, "message") != errorField(t, foreign, "message") {
		t.Errorf("messages differ: %v vs %v", unknown, foreign)
	}
	if n := len(e.feedbackRows(e.proj.ID)) + len(e.feedbackRows(other.ID)); n != 0 {
		t.Errorf("%d rows written for ids the project does not own", n)
	}
}

func TestFeedbackUpsertLastWriterWins(t *testing.T) {
	e := newEnv(t, nil)
	e.logged(e.proj.ID, "req-1")
	for _, b := range []map[string]any{
		{"request_id": "req-1", "score": 1, "label": "helpful"},
		{"request_id": "req-1", "score": -1, "label": "wrong_answer"},
	} {
		if resp, out := e.rate(e.key, b); resp.StatusCode != http.StatusOK {
			t.Fatalf("%v: %d %v", b, resp.StatusCode, out)
		}
	}
	rows := e.feedbackRows(e.proj.ID)
	if len(rows) != 1 || rows["req-1"].Score != -1 || *rows["req-1"].Label != "wrong_answer" {
		t.Errorf("rows = %+v, want the second submission only", rows)
	}
	// A resubmission without a label clears the label: it replaces, not merges.
	if resp, _ := e.rate(e.key, map[string]any{"request_id": "req-1", "score": 1}); resp.StatusCode != 200 {
		t.Fatal("resubmission failed")
	}
	if rows = e.feedbackRows(e.proj.ID); rows["req-1"].Score != 1 || rows["req-1"].Label != nil {
		t.Errorf("rows = %+v", rows)
	}
	// created_at stays, updated_at moves.
	var created, updated time.Time
	if err := e.st.DB().QueryRow(context.Background(),
		"SELECT created_at, updated_at FROM request_feedback WHERE project_id = $1", e.proj.ID).Scan(&created, &updated); err != nil {
		t.Fatal(err)
	}
	if !updated.After(created) {
		t.Errorf("created %v updated %v, want updated_at after created_at", created, updated)
	}
}

// request_id is not unique (a client may choose it), so two rows that share
// one carry one feedback.
func TestFeedbackSharedRequestIDIsOneFeedback(t *testing.T) {
	e := newEnv(t, nil)
	e.logged(e.proj.ID, "shared")
	e.logged(e.proj.ID, "shared")
	if resp, out := e.rate(e.key, map[string]any{"request_id": "shared", "score": 1}); resp.StatusCode != 200 {
		t.Fatalf("%d %v", resp.StatusCode, out)
	}
	if resp, out := e.rate(e.key, map[string]any{"request_id": "shared", "score": -1}); resp.StatusCode != 200 {
		t.Fatalf("%d %v", resp.StatusCode, out)
	}
	if rows := e.feedbackRows(e.proj.ID); len(rows) != 1 || rows["shared"].Score != -1 {
		t.Errorf("rows = %+v, want one feedback", rows)
	}
}

// The same id in two projects is two feedbacks, each hung on its own project.
func TestFeedbackIsPerProject(t *testing.T) {
	e := newEnv(t, nil)
	other := e.newProject("other", nil)
	e.logged(e.proj.ID, "same")
	e.logged(other.ID, "same")
	_, otherKey := e.userKey("other", []int64{other.ID}, nil)
	if resp, _ := e.rate(e.key, map[string]any{"request_id": "same", "score": 1}); resp.StatusCode != 200 {
		t.Fatal("first project")
	}
	if resp, _ := e.rate(otherKey, map[string]any{"request_id": "same", "score": -1}); resp.StatusCode != 200 {
		t.Fatal("second project")
	}
	if a, b := e.feedbackRows(e.proj.ID)["same"], e.feedbackRows(other.ID)["same"]; a.Score != 1 || b.Score != -1 {
		t.Errorf("scores = %d and %d, want 1 and -1", a.Score, b.Score)
	}
}

func TestFeedbackScopes(t *testing.T) {
	e := newEnv(t, nil)
	e.logged(e.proj.ID, "req-1")
	body := map[string]any{"request_id": "req-1", "score": 1}
	for name, c := range map[string]struct {
		scopes []string
		want   int
	}{
		"chat":                {[]string{store.ScopeChat}, 200},
		"retrieve":            {[]string{store.ScopeRetrieve}, 200},
		"chat and models":     {[]string{store.ScopeChat, store.ScopeModels}, 200},
		"models only":         {[]string{store.ScopeModels}, 403},
		"embeddings only":     {[]string{store.ScopeEmbeddings}, 403},
		"rerank and models":   {[]string{store.ScopeRerank, store.ScopeModels}, 403},
		"embeddings+retrieve": {[]string{store.ScopeEmbeddings, store.ScopeRetrieve}, 200},
	} {
		_, raw := e.userKey(name, []int64{e.proj.ID}, func(k *store.APIKey) { k.Scopes = c.scopes })
		resp, out := e.rate(raw, body)
		if resp.StatusCode != c.want {
			t.Errorf("%s: %d %v, want %d", name, resp.StatusCode, out, c.want)
		}
		if c.want == 403 && (errorField(t, out, "type") != "insufficient_scope" || errorField(t, out, "code") != "insufficient_scope") {
			t.Errorf("%s: %v", name, out)
		}
	}
	// The project's own key carries no scopes and is not narrowed by any.
	if resp, _ := e.rate(e.key, body); resp.StatusCode != 200 {
		t.Errorf("project key: %d", resp.StatusCode)
	}
	// Authentication comes first.
	if resp, _ := e.rate("sk-proj-nope", body); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("bad key: %d, want 401", resp.StatusCode)
	}
	// A key that grants two projects must name one.
	other := e.newProject("other", nil)
	_, both := e.userKey("both", []int64{e.proj.ID, other.ID}, nil)
	resp, out := e.rate(both, body)
	if resp.StatusCode != http.StatusBadRequest || errorField(t, out, "code") != "project_required" {
		t.Errorf("two grants, none named: %d %v", resp.StatusCode, out)
	}
	resp, _ = e.call(http.MethodPost, "/v1/feedback", body, both, map[string]string{projectHeader: e.proj.Name})
	if resp.StatusCode != 200 {
		t.Errorf("two grants, one named: %d", resp.StatusCode)
	}
	// Only POST.
	if resp, _ := e.call(http.MethodGet, "/v1/feedback", nil, e.key, nil); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET: %d, want 405", resp.StatusCode)
	}
}

// Feedback is not a pipeline call: no slot of the request limit, no request
// log row (ADR-042).
func TestFeedbackIsNotCountedAndLeavesNoRow(t *testing.T) {
	e := newEnv(t, func(p *store.Project) { p.RateLimitRPM = 1; p.RateLimitTPM = 1000 })
	e.logged(e.proj.ID, "req-1")
	before := e.requestLogCount()
	for i := 0; i < 5; i++ {
		if resp, out := e.rate(e.key, map[string]any{"request_id": "req-1", "score": 1}); resp.StatusCode != 200 {
			t.Fatalf("feedback %d: %d %v (a limit counted it?)", i, resp.StatusCode, out)
		}
	}
	// A refused one counts for nothing either.
	e.rate(e.key, map[string]any{"request_id": "nope", "score": 1})
	e.rate(e.key, map[string]any{"request_id": "req-1", "score": 5})
	if after := e.requestLogCount(); after != before {
		t.Errorf("request_logs: %d rows after feedback, %d before", after, before)
	}
	// The single request of the minute is still there for a real call.
	resp, out := e.chat(map[string]any{"messages": userMsg})
	if resp.StatusCode != 200 || resp.Header.Get("x-ratelimit-remaining-requests") != "0" {
		t.Errorf("chat after feedback: %d remaining %q %v; the feedback used the minute's request",
			resp.StatusCode, resp.Header.Get("x-ratelimit-remaining-requests"), out)
	}
}

// Deleting the sending key keeps the feedback and forgets who sent it.
func TestFeedbackSurvivesItsKey(t *testing.T) {
	e := newEnv(t, nil)
	e.logged(e.proj.ID, "req-1")
	k, raw := e.userKey("temp", []int64{e.proj.ID}, nil)
	if resp, _ := e.rate(raw, map[string]any{"request_id": "req-1", "score": 1}); resp.StatusCode != 200 {
		t.Fatal("feedback")
	}
	if _, err := e.st.DB().Exec(context.Background(), "DELETE FROM api_keys WHERE id = $1", k.ID); err != nil {
		t.Fatal(err)
	}
	if got := e.feedbackRows(e.proj.ID)["req-1"]; got.Score != 1 || got.Key != nil {
		t.Errorf("row = %+v, want the score kept and no key", got)
	}
}
