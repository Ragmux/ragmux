package e2e

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ragmux/ragmux/internal/testdb"
)

// countingUpstream answers every chat call with a fixed reply and counts them.
func countingUpstream(hits *atomic.Int64) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write(mustJSON(map[string]any{"id": "x", "model": "m",
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "pong"}, "finish_reason": "stop"}},
			"usage":   map[string]int{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}}))
	}))
}

func errMessage(r map[string]any) string {
	if e, ok := r["error"].(map[string]any); ok {
		s, _ := e["message"].(string)
		return s
	}
	return ""
}

func TestRoutesAdminAPIAndGateway(t *testing.T) {
	var hitsA, hitsB atomic.Int64
	upA, upB := countingUpstream(&hitsA), countingUpstream(&hitsB)
	defer upA.Close()
	defer upB.Close()
	e := newEnv(t, testdb.Config(t))

	mkConn := func(name, url, model, kind string) int64 {
		c := e.call("POST", "/admin/api/models", map[string]any{"name": name, "provider_type": kind,
			"base_url": url + "/v1", "api_key": "k", "model_name": model}, "")
		if c["_status"] != float64(201) {
			t.Fatalf("create connection %s: %v", name, c)
		}
		return int64(c["id"].(float64))
	}
	connA := mkConn("a", upA.URL, "model-a", "custom_openai")
	connB := mkConn("b", upB.URL, "model-b", "custom_openai")

	proj := e.call("POST", "/admin/api/projects", map[string]any{"name": "routed", "model_connection_id": connA}, "")
	if proj["_status"] != float64(201) {
		t.Fatalf("create project: %v", proj)
	}
	apiKey := proj["api_key"].(string)
	pid := int64(proj["project"].(map[string]any)["id"].(float64))
	base := fmt.Sprintf("/admin/api/projects/%d/routes", pid)

	// Validation.
	bad := []struct {
		name string
		body map[string]any
	}{
		{"no name", map[string]any{"targets": []any{map[string]any{"connection_id": connA}}}},
		{"no targets", map[string]any{"name": "r", "targets": []any{}}},
		{"unknown kind", map[string]any{"name": "r", "kind": "images", "targets": []any{map[string]any{"connection_id": connA}}}},
		{"unknown strategy", map[string]any{"name": "r", "strategy": "random", "targets": []any{map[string]any{"connection_id": connA}}}},
		{"missing connection", map[string]any{"name": "r", "targets": []any{map[string]any{"connection_id": 999999}}}},
		{"zero weight", map[string]any{"name": "r", "targets": []any{map[string]any{"connection_id": connA, "weight": 0}}}},
		{"huge weight", map[string]any{"name": "r", "targets": []any{map[string]any{"connection_id": connA, "weight": 1001}}}},
		{"huge priority", map[string]any{"name": "r", "targets": []any{map[string]any{"connection_id": connA, "priority": 101}}}},
		{"duplicate target", map[string]any{"name": "r", "targets": []any{map[string]any{"connection_id": connA}, map[string]any{"connection_id": connA}}}},
		{"rerank on chat provider", map[string]any{"name": "r", "kind": "rerank", "targets": []any{map[string]any{"connection_id": connA}}}},
	}
	for _, c := range bad {
		if r := e.call("POST", base, c.body, ""); r["_status"] != float64(400) {
			t.Errorf("%s: want 400, got %v", c.name, r)
		}
	}
	many := make([]any, 9)
	for i := range many {
		many[i] = map[string]any{"connection_id": connA, "model": fmt.Sprintf("m%d", i)}
	}
	if r := e.call("POST", base, map[string]any{"name": "r", "targets": many}, ""); r["_status"] != float64(400) {
		t.Errorf("9 targets: want 400, got %v", r)
	}

	// Create: defaults fill in kind, strategy, weight and priority.
	created := e.call("POST", base, map[string]any{"name": "smart", "targets": []any{
		map[string]any{"connection_id": connB, "model": "big-b"}}}, "")
	if created["_status"] != float64(201) || created["kind"] != "chat" || created["strategy"] != "weighted" {
		t.Fatalf("create route: %v", created)
	}
	tg := created["targets"].([]any)[0].(map[string]any)
	if tg["weight"] != float64(1) || tg["priority"] != float64(0) || tg["model"] != "big-b" || tg["provider_type"] != "custom_openai" {
		t.Errorf("target defaults: %v", tg)
	}
	rid := int64(created["id"].(float64))
	one := fmt.Sprintf("%s/%d", base, rid)

	if dup := e.call("POST", base, map[string]any{"name": "smart", "targets": []any{map[string]any{"connection_id": connA}}}, ""); dup["_status"] != float64(409) {
		t.Errorf("duplicate name: %v", dup)
	}
	if list := e.call("GET", base, nil, ""); list["_status"] != float64(200) || len(list["_list"].([]any)) != 1 {
		t.Errorf("list: %v", list)
	}
	if got := e.call("GET", one, nil, ""); got["name"] != "smart" || len(got["targets"].([]any)) != 1 {
		t.Errorf("get: %v", got)
	}
	if r := e.call("GET", base+"/999999", nil, ""); r["_status"] != float64(404) {
		t.Errorf("unknown route: %v", r)
	}
	if r := e.call("GET", base+"/9999999999", nil, ""); r["_status"] != float64(404) {
		t.Errorf("route id past int32 must be 404: %v", r)
	}

	// The gateway now routes: the route name goes to connB, the implicit
	// name goes to connA, anything else is model_not_found.
	chat := func(model string) map[string]any {
		body := map[string]any{"messages": []map[string]string{{"role": "user", "content": "ping"}}}
		if model != "" {
			body["model"] = model
		}
		return e.call("POST", "/v1/chat/completions", body, apiKey)
	}
	if r := chat("smart"); r["_status"] != float64(200) || r["model"] != "smart" || hitsB.Load() != 1 || hitsA.Load() != 0 {
		t.Fatalf("route call: %v hits a=%d b=%d", r, hitsA.Load(), hitsB.Load())
	}
	for _, m := range []string{"model-a", ""} {
		if r := chat(m); r["_status"] != float64(200) {
			t.Fatalf("implicit %q: %v", m, r)
		}
	}
	if hitsA.Load() != 2 {
		t.Errorf("implicit route calls = %d, want 2", hitsA.Load())
	}
	r := chat("ghost")
	if r["_status"] != float64(404) || r["error"].(map[string]any)["code"] != "model_not_found" {
		t.Errorf("unknown model: %v", r)
	}
	models := e.call("GET", "/v1/models", nil, apiKey)
	if models["_status"] != float64(200) || len(models["data"].([]any)) != 2 {
		t.Errorf("/v1/models: %v", models)
	}

	// A connection used by a route target cannot be deleted. connB is in no
	// project, so only the route holds it.
	del := e.call("DELETE", fmt.Sprintf("/admin/api/models/%d", connB), nil, "")
	if del["_status"] != float64(409) || !strings.Contains(errMessage(del), "project, RAG store or route") {
		t.Errorf("delete connection in a route: %v", del)
	}

	// Update replaces the targets as a whole.
	upd := e.call("PUT", one, map[string]any{"name": "smart", "targets": []any{
		map[string]any{"connection_id": connA, "weight": 5, "priority": 1},
		map[string]any{"connection_id": connB, "weight": 2, "priority": 0}}}, "")
	if upd["_status"] != float64(200) || len(upd["targets"].([]any)) != 2 {
		t.Fatalf("update: %v", upd)
	}
	if first := upd["targets"].([]any)[0].(map[string]any); first["priority"] != float64(0) {
		t.Errorf("targets are not returned in priority order: %v", upd["targets"])
	}
	if r := e.call("PUT", base+"/999999", map[string]any{"name": "x", "targets": []any{map[string]any{"connection_id": connA}}}, ""); r["_status"] != float64(404) {
		t.Errorf("update unknown route: %v", r)
	}
	if r := e.call("PUT", one, map[string]any{"name": "smart", "targets": []any{}}, ""); r["_status"] != float64(400) {
		t.Errorf("update with no targets: %v", r)
	}
	if got := e.call("GET", one, nil, ""); len(got["targets"].([]any)) != 2 {
		t.Errorf("a rejected update changed the route: %v", got)
	}

	// A connection a chat route uses cannot become a provider that cannot chat.
	connPath := fmt.Sprintf("/admin/api/models/%d", connA)
	if r := e.call("PUT", connPath, map[string]any{"name": "a", "provider_type": "cohere_rerank",
		"base_url": upA.URL + "/v1", "model_name": "model-a"}, ""); r["_status"] != float64(409) {
		t.Errorf("provider change under a chat route: %v", r)
	}
	if r := e.call("PUT", connPath, map[string]any{"name": "a", "provider_type": "custom_openai",
		"base_url": upA.URL + "/v1", "model_name": "model-a"}, ""); r["_status"] != float64(200) {
		t.Errorf("a compatible update must pass: %v", r)
	}

	// Delete frees the connection again.
	if r := e.call("DELETE", one, nil, ""); r["_status"] != float64(200) {
		t.Fatalf("delete route: %v", r)
	}
	if r := e.call("DELETE", one, nil, ""); r["_status"] != float64(404) {
		t.Errorf("second delete: %v", r)
	}
	if r := chat("smart"); r["_status"] != float64(200) || r["model"] != "smart" {
		t.Errorf("a route-less project must echo again: %v", r)
	}
	if r := e.call("DELETE", fmt.Sprintf("/admin/api/models/%d", connB), nil, ""); r["_status"] != float64(200) {
		t.Errorf("delete freed connection: %v", r)
	}

	// Audit.
	for _, action := range []string{"route.create", "route.update", "route.delete"} {
		a := e.call("GET", "/admin/api/audit?action="+action, nil, "")
		if a["total"] != float64(1) {
			t.Errorf("audit %s: %v", action, a)
		}
	}
}

func TestRoutesRespectProjectMembership(t *testing.T) {
	var hits atomic.Int64
	up := countingUpstream(&hits)
	defer up.Close()
	e := newEnv(t, testdb.Config(t))
	conn := e.call("POST", "/admin/api/models", map[string]any{"name": "a", "provider_type": "custom_openai",
		"base_url": up.URL + "/v1", "api_key": "k", "model_name": "m"}, "")
	connID := int64(conn["id"].(float64))
	mkProject := func(name string) int64 {
		p := e.call("POST", "/admin/api/projects", map[string]any{"name": name, "model_connection_id": connID}, "")
		return int64(p["project"].(map[string]any)["id"].(float64))
	}
	shared, private := mkProject("shared"), mkProject("private")
	e.call("POST", "/admin/api/users", map[string]any{"username": "ed", "password": "editor-pass-1", "role": "editor", "project_ids": []int64{shared}}, "")
	e.call("POST", "/admin/api/users", map[string]any{"username": "vw", "password": "viewer-pass-1", "role": "viewer", "project_ids": []int64{shared}}, "")
	editorTok, viewerTok := e.login("ed", "editor-pass-1"), e.login("vw", "viewer-pass-1")

	body := map[string]any{"name": "r", "targets": []any{map[string]any{"connection_id": connID}}}
	sharedBase := fmt.Sprintf("/admin/api/projects/%d/routes", shared)
	privateBase := fmt.Sprintf("/admin/api/projects/%d/routes", private)

	if r := e.call("GET", privateBase, nil, editorTok); httpStatus(r) != 404 {
		t.Errorf("non-member list: %v", r)
	}
	if r := e.call("POST", privateBase, body, editorTok); httpStatus(r) != 404 {
		t.Errorf("non-member create: %v", r)
	}
	if r := e.call("POST", sharedBase, body, viewerTok); httpStatus(r) != 403 {
		t.Errorf("viewer create: %v", r)
	}
	if r := e.call("POST", sharedBase, body, editorTok); httpStatus(r) != 201 {
		t.Fatalf("member editor create: %v", r)
	}
	if r := e.call("GET", sharedBase, nil, viewerTok); httpStatus(r) != 200 || len(r["_list"].([]any)) != 1 {
		t.Errorf("viewer list: %v", r)
	}
}

func httpStatus(r map[string]any) int {
	s, _ := r["_status"].(float64)
	return int(s)
}
