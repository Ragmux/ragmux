package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ragmux/ragmux/internal/limits"
	"github.com/ragmux/ragmux/internal/store"
)

// pipelineRequest builds a request whose context carries what the identify
// middleware would have put there, so a step can be driven on its own.
func pipelineRequest(pr *principal) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	ctx := context.WithValue(r.Context(), principalKey{}, pr)
	ctx = context.WithValue(ctx, ctxKey{}, pr.project)
	return r.WithContext(ctx)
}

// routedCall runs authenticate and resolveRoute for the project key.
func (e *env) routedCall(w *httptest.ResponseRecorder) *call {
	e.t.Helper()
	c, ok := e.gw.authenticate(w, pipelineRequest(&principal{project: e.proj}), chatOperation)
	if !ok {
		e.t.Fatalf("authenticate refused: %d %s", w.Code, w.Body.String())
	}
	if !e.gw.resolveRoute(c) {
		e.t.Fatalf("resolveRoute refused: %d %s", w.Code, w.Body.String())
	}
	return c
}

func TestAuthenticateBuildsCall(t *testing.T) {
	e := newEnv(t, nil)
	w := httptest.NewRecorder()
	c, ok := e.gw.authenticate(w, pipelineRequest(&principal{project: e.proj}), chatOperation)
	if !ok {
		t.Fatalf("authenticate refused: %d %s", w.Code, w.Body.String())
	}
	defer c.span.End()
	if c.project.ID != e.proj.ID || c.projectID == "" || c.subject.Project == nil || c.subject.KeyID != nil {
		t.Errorf("call = %+v", c)
	}
	if c.op.name != "chat" || c.obs == nil || c.start.IsZero() || c.conn != nil || c.rec != nil {
		t.Errorf("call state = %+v", c)
	}
	if w.Body.Len() != 0 {
		t.Errorf("a successful authenticate wrote %q", w.Body.String())
	}
}

func TestAuthenticateCarriesKeyIntoSubject(t *testing.T) {
	e := newEnv(t, nil)
	key, _ := e.userKey("k", []int64{e.proj.ID}, nil)
	w := httptest.NewRecorder()
	c, ok := e.gw.authenticate(w, pipelineRequest(&principal{project: e.proj, key: key, user: e.owner}), chatOperation)
	if !ok {
		t.Fatalf("authenticate refused: %d %s", w.Code, w.Body.String())
	}
	defer c.span.End()
	if c.subject.KeyID == nil || *c.subject.KeyID != key.ID {
		t.Errorf("subject = %+v, want key %d", c.subject, key.ID)
	}
}

func TestAuthenticateRefusesMissingScope(t *testing.T) {
	e := newEnv(t, nil)
	key, _ := e.userKey("models-only", []int64{e.proj.ID}, func(k *store.APIKey) { k.Scopes = []string{store.ScopeModels} })
	w := httptest.NewRecorder()
	c, ok := e.gw.authenticate(w, pipelineRequest(&principal{project: e.proj, key: key, user: e.owner}), chatOperation)
	if ok || c != nil {
		t.Fatal("authenticate accepted a key without the chat scope")
	}
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"code":"insufficient_scope"`) ||
		!strings.Contains(w.Body.String(), "api key is not authorised for chat completions") {
		t.Errorf("refusal = %d %s", w.Code, w.Body.String())
	}
}

func TestAuthenticateRefusesUnroutableKey(t *testing.T) {
	e := newEnv(t, nil)
	second := e.newProject("second", nil)

	// Several grants and none named: the client is told which to pick.
	w := httptest.NewRecorder()
	_, ok := e.gw.authenticate(w, pipelineRequest(&principal{grants: []*store.Project{e.proj, second}}), chatOperation)
	if ok || w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"code":"project_required"`) ||
		w.Header().Get(projectsHeader) != e.proj.Name+","+second.Name {
		t.Errorf("project_required = %d %v %s", w.Code, w.Header(), w.Body.String())
	}

	// No grants left: it fails closed.
	w = httptest.NewRecorder()
	_, ok = e.gw.authenticate(w, pipelineRequest(&principal{}), chatOperation)
	if ok || w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"code":"project_not_granted"`) {
		t.Errorf("project_not_granted = %d %s", w.Code, w.Body.String())
	}
}

func TestResolveRouteSetsConnection(t *testing.T) {
	e := newEnv(t, nil)
	c := e.routedCall(httptest.NewRecorder())
	defer c.span.End()
	if c.conn == nil || c.conn.ID != e.conn.ID || c.conn.ModelName != "mock-model" {
		t.Errorf("conn = %+v", c.conn)
	}
}

func TestResolveRouteMissingConnection(t *testing.T) {
	e := newEnv(t, nil)
	orphan := *e.proj
	orphan.ModelConnectionID = e.conn.ID + 1000
	w := httptest.NewRecorder()
	c, ok := e.gw.authenticate(w, pipelineRequest(&principal{project: &orphan}), chatOperation)
	if !ok {
		t.Fatalf("authenticate refused: %d", w.Code)
	}
	defer c.span.End()
	if e.gw.resolveRoute(c) || c.conn != nil {
		t.Fatal("resolveRoute accepted a missing connection")
	}
	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "model connection unavailable") {
		t.Errorf("answer = %d %s", w.Code, w.Body.String())
	}
}

func TestAcceptTargetsRefusesAnEmptyRoute(t *testing.T) {
	e := newEnv(t, nil)
	w := httptest.NewRecorder()
	c, ok := e.gw.authenticate(w, pipelineRequest(&principal{project: e.proj}), chatOperation)
	if !ok {
		t.Fatalf("authenticate refused: %d", w.Code)
	}
	defer c.span.End()
	if acceptTargets(c, nil) || c.conn != nil || len(c.targets) != 0 {
		t.Fatalf("an empty route was accepted: conn = %v targets = %v", c.conn, c.targets)
	}
	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "model connection unavailable") {
		t.Errorf("answer = %d %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	c.w = w
	one, two := *e.conn, *e.conn
	if !acceptTargets(c, []*store.ModelConnection{&one, &two}) || c.conn != &one || len(c.targets) != 2 || w.Body.Len() != 0 {
		t.Errorf("conn = %v targets = %d body = %q", c.conn, len(c.targets), w.Body.String())
	}
}

func TestCheckLimitsAllowsAndDenies(t *testing.T) {
	e := newEnv(t, func(p *store.Project) { p.RateLimitRPM = 1 })

	w := httptest.NewRecorder()
	c := e.routedCall(w)
	defer c.span.End()
	if !e.gw.checkLimits(c, 10, false) {
		t.Fatalf("first call denied: %d %s", w.Code, w.Body.String())
	}
	if !c.decision.Allowed || !c.decision.Reserved || w.Header().Get("x-ratelimit-limit-requests") != "1" {
		t.Errorf("decision = %+v headers = %v", c.decision, w.Header())
	}

	w = httptest.NewRecorder()
	c = e.routedCall(w)
	defer c.span.End()
	if e.gw.checkLimits(c, 10, true) {
		t.Fatal("second call within the minute was allowed")
	}
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" ||
		!strings.Contains(w.Body.String(), `"code":"`+limits.ReasonRPM+`"`) {
		t.Errorf("denial = %d %v %s", w.Code, w.Header(), w.Body.String())
	}
	rec := e.lastLog()
	if rec.StatusCode != http.StatusTooManyRequests || rec.Error != limits.ReasonRPM || !rec.Streamed ||
		rec.CostSource != store.CostSourceNone || rec.ModelName != "mock-model" {
		t.Errorf("denial row = %+v", rec)
	}
}

func TestCheckLimitsWithoutLimiter(t *testing.T) {
	e := newEnv(t, nil)
	e.gw.Limiter = nil
	w := httptest.NewRecorder()
	c := e.routedCall(w)
	defer c.span.End()
	if !e.gw.checkLimits(c, 10, false) || w.Header().Get("x-ratelimit-limit-requests") != "" {
		t.Errorf("checkLimits without a limiter: %v", w.Header())
	}
}

func TestOpenRecordAndRecordWriteTheRow(t *testing.T) {
	e := newEnv(t, nil)
	key, _ := e.userKey("k", []int64{e.proj.ID}, nil)
	ctx := context.Background()
	if before, err := e.st.GetAPIKey(ctx, key.ID); err != nil || before.LastUsedAt != nil {
		t.Fatalf("key before = %+v, %v", before, err)
	}

	w := httptest.NewRecorder()
	c, ok := e.gw.authenticate(w, pipelineRequest(&principal{project: e.proj, key: key, user: e.owner}), chatOperation)
	if !ok || !e.gw.resolveRoute(c) {
		t.Fatalf("setup refused: %d %s", w.Code, w.Body.String())
	}
	e.gw.openRecord(c, false, func(rec *store.RequestLog) { rec.RAGUsed, rec.RAGHits = true, 4 })
	if c.rec == nil || c.rec.ProjectID != e.proj.ID || c.rec.ModelName != "mock-model" || c.rec.Streamed {
		t.Fatalf("rec = %+v", c.rec)
	}
	if c.rec.APIKeyID == nil || *c.rec.APIKeyID != key.ID {
		t.Errorf("row not attributed to the key: %+v", c.rec)
	}
	if !c.rec.RAGUsed || c.rec.RAGHits != 4 {
		t.Errorf("fill callback not applied: %+v", c.rec)
	}
	c.rec.StatusCode, c.rec.PromptTokens, c.rec.CompletionTokens = http.StatusOK, 7, 3
	e.gw.record(c)
	c.span.End()

	rec := e.lastLog()
	if rec.StatusCode != http.StatusOK || rec.PromptTokens != 7 || rec.CompletionTokens != 3 || !rec.RAGUsed || rec.RAGHits != 4 {
		t.Errorf("row = %+v", rec)
	}

	// The limiter counted the request and its tokens, for the project and for
	// the key.
	now := time.Now()
	_, day, _, err := e.st.GetUsage(ctx, e.proj.ID, limits.Windows(now))
	if err != nil {
		t.Fatal(err)
	}
	if day.Requests != 1 || day.PromptTokens != 7 || day.CompletionTokens != 3 {
		t.Errorf("project usage = %+v", day)
	}
	_, kday, _, err := e.st.GetKeyUsage(ctx, key.ID, limits.Windows(now))
	if err != nil {
		t.Fatal(err)
	}
	if kday.Requests != 1 || kday.PromptTokens != 7 || kday.CompletionTokens != 3 {
		t.Errorf("key usage = %+v", kday)
	}

	// The key's last_used_at was stamped.
	after, err := e.st.GetAPIKey(ctx, key.ID)
	if err != nil || after.LastUsedAt == nil {
		t.Errorf("last_used_at not stamped: %+v, %v", after, err)
	}
}

// fakeUpstream is an upstreamCall that plays back a canned result.
type fakeUpstream struct {
	calls   int
	results []error
	seen    []*store.ModelConnection
	// settle, if set, answers the client the way a settled attempt does.
	settle func(c *call)
}

func (f *fakeUpstream) attempt(c *call, conn *store.ModelConnection) error {
	f.seen = append(f.seen, conn)
	err := f.results[f.calls]
	f.calls++
	if err == nil && f.settle != nil {
		f.settle(c)
	}
	return err
}

// invokeCall is a routed call with an open record row, ready for invoke.
func (e *env) invokeCall(w *httptest.ResponseRecorder) *call {
	e.t.Helper()
	c := e.routedCall(w)
	e.gw.openRecord(c, false, nil)
	return c
}

func TestInvokeSettledAttemptWritesNothingItself(t *testing.T) {
	e := newEnv(t, nil)
	w := httptest.NewRecorder()
	c := e.invokeCall(w)
	defer c.span.End()
	up := &fakeUpstream{results: []error{nil}, settle: func(c *call) {
		c.rec.StatusCode = http.StatusOK
		_, _ = c.w.Write([]byte("ok"))
	}}
	e.gw.invoke(c, up)
	if up.calls != 1 || up.seen[0] != c.targets[0] {
		t.Fatalf("calls = %d seen = %v", up.calls, up.seen)
	}
	if w.Code != http.StatusOK || w.Body.String() != "ok" || c.rec.StatusCode != http.StatusOK || c.obs.errType != "" {
		t.Errorf("response = %d %q rec = %+v obs = %+v", w.Code, w.Body.String(), c.rec, c.obs)
	}
}

func TestInvokeAnswersAProviderFailure(t *testing.T) {
	e := newEnv(t, nil)
	w := httptest.NewRecorder()
	c := e.invokeCall(w)
	defer c.span.End()
	up := &fakeUpstream{results: []error{errors.New("boom")}}
	e.gw.invoke(c, up)
	if up.calls != 1 {
		t.Fatalf("attempts = %d, want 1 (no retry yet)", up.calls)
	}
	if w.Code < 500 || c.rec.StatusCode != w.Code || c.rec.Error == "" || c.obs.errType == "" {
		t.Errorf("response = %d %s rec = %+v obs = %+v", w.Code, w.Body.String(), c.rec, c.obs)
	}
	if !strings.Contains(w.Body.String(), `"error"`) {
		t.Errorf("body is not an error envelope: %s", w.Body.String())
	}
}

func TestInvokeTreatsACancelledRequestAsClientClosed(t *testing.T) {
	e := newEnv(t, nil)
	w := httptest.NewRecorder()
	c := e.invokeCall(w)
	defer c.span.End()
	ctx, cancel := context.WithCancel(c.r.Context())
	c.r = c.r.WithContext(ctx)
	cancel()
	e.gw.invoke(c, &fakeUpstream{results: []error{context.Canceled}})
	if c.rec.StatusCode != statusClientClosed || c.rec.Error != errClientClosed {
		t.Errorf("rec = %+v", c.rec)
	}
	if w.Body.Len() != 0 {
		t.Errorf("a gone client was written to: %q", w.Body.String())
	}
}

func TestInvokeStopsAtTheFirstFailure(t *testing.T) {
	e := newEnv(t, nil)
	w := httptest.NewRecorder()
	c := e.invokeCall(w)
	defer c.span.End()
	first := c.targets[0]
	second := *first
	second.ModelName = "second"
	c.targets = append(c.targets, &second)
	up := &fakeUpstream{results: []error{errors.New("first down"), nil}}
	e.gw.invoke(c, up)
	if up.calls != 1 || up.seen[0] != first || c.conn != first {
		t.Errorf("calls = %d conn = %v, want one attempt at the first target", up.calls, c.conn)
	}
	if w.Code < 500 || c.rec.StatusCode != w.Code {
		t.Errorf("response = %d rec = %+v", w.Code, c.rec)
	}
}

func TestInvokeWithoutTargetsAnswers500(t *testing.T) {
	e := newEnv(t, nil)
	w := httptest.NewRecorder()
	c := e.invokeCall(w)
	defer c.span.End()
	c.targets = nil
	up := &fakeUpstream{}
	e.gw.invoke(c, up)
	if up.calls != 0 || w.Code != http.StatusInternalServerError || c.rec.StatusCode != http.StatusInternalServerError ||
		c.rec.Error == "" || c.obs.errType != "server_error" {
		t.Errorf("response = %d %s rec = %+v obs = %+v", w.Code, w.Body.String(), c.rec, c.obs)
	}
}
