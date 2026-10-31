package admin

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/ragmux/ragmux/internal/store"
)

func TestProjectToolSettings(t *testing.T) {
	e := newAdminEnv(t)
	editor := e.user("editor", "editor")
	base := func(extra map[string]any) map[string]any {
		body := map[string]any{"name": "agents", "model_connection_id": e.conn.ID}
		for k, v := range extra {
			body[k] = v
		}
		return body
	}

	status, out := e.do(http.MethodPost, "/api/projects", base(nil), editor.token)
	if status != http.StatusCreated {
		t.Fatalf("create: %d %v", status, out)
	}
	proj, _ := out["project"].(map[string]any)
	if allow, ok := proj["tool_allow"].([]any); !ok || len(allow) != 0 || proj["session_token_cap"] != float64(0) {
		t.Errorf("defaults = %v, want empty lists and a zero cap (not null)", proj)
	}
	id := itoa(int64(proj["id"].(float64)))

	// Set all three.
	status, out = e.do(http.MethodPut, "/api/projects/"+id, base(map[string]any{
		"tool_allow": []string{"search", "mcp__docs__*"}, "tool_deny": []string{"mcp__github__*", "mcp__github__*"},
		"session_token_cap": 50000}), editor.token)
	if status != http.StatusOK {
		t.Fatalf("update: %d %v", status, out)
	}
	if !reflect.DeepEqual(out["tool_deny"], []any{"mcp__github__*"}) || out["session_token_cap"] != float64(50000) ||
		!reflect.DeepEqual(out["tool_allow"], []any{"search", "mcp__docs__*"}) {
		t.Errorf("updated = %v (duplicates collapse)", out)
	}

	// A body that leaves them out keeps them.
	status, out = e.do(http.MethodPut, "/api/projects/"+id, base(map[string]any{"system_prompt": "hi"}), editor.token)
	if status != http.StatusOK || out["session_token_cap"] != float64(50000) || len(out["tool_allow"].([]any)) != 2 {
		t.Errorf("left out: %d %v, want the settings kept", status, out)
	}

	// An empty list clears, a zero cap lifts the cap.
	status, out = e.do(http.MethodPut, "/api/projects/"+id, base(map[string]any{
		"tool_allow": []string{}, "session_token_cap": 0}), editor.token)
	if status != http.StatusOK || len(out["tool_allow"].([]any)) != 0 || out["session_token_cap"] != float64(0) ||
		len(out["tool_deny"].([]any)) != 1 {
		t.Errorf("clear: %d %v", status, out)
	}

	tooMany := make([]string, 65)
	for i := range tooMany {
		tooMany[i] = "t" + strings.Repeat("x", i%5) + string(rune('a'+i/26)) + string(rune('a'+i%26))
	}
	for name, extra := range map[string]map[string]any{
		"regexp":         {"tool_deny": []string{"a.+"}},
		"space":          {"tool_allow": []string{"a b"}},
		"empty pattern":  {"tool_deny": []string{""}},
		"too long":       {"tool_deny": []string{strings.Repeat("a", 129)}},
		"too many":       {"tool_deny": tooMany},
		"negative cap":   {"session_token_cap": -1},
		"slash":          {"tool_deny": []string{"a/b"}},
		"non ASCII name": {"tool_deny": []string{"araç"}},
	} {
		status, out := e.do(http.MethodPut, "/api/projects/"+id, base(extra), editor.token)
		if status != http.StatusBadRequest {
			t.Errorf("%s: status %d %v, want 400", name, status, out)
		}
		status, _ = e.do(http.MethodPost, "/api/projects", base(extra), editor.token)
		if status != http.StatusBadRequest {
			t.Errorf("%s on create: status %d, want 400", name, status)
		}
	}
	// 64 distinct patterns are fine.
	status, out = e.do(http.MethodPut, "/api/projects/"+id, base(map[string]any{"tool_deny": tooMany[:64]}), editor.token)
	if status != http.StatusOK {
		t.Errorf("64 patterns: %d %v", status, out)
	}
}

func TestSessionEndpoints(t *testing.T) {
	e := newAdminEnv(t)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := e.st.RecordSessionUsage(ctx, e.proj.ID, "run-1", 10, 5); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.st.RecordSessionUsage(ctx, e.proj.ID, "run-2", 1, 1); err != nil {
		t.Fatal(err)
	}
	if err := e.st.RecordSessionUsage(ctx, e.other.ID, "secret-run", 1, 1); err != nil {
		t.Fatal(err)
	}
	if err := e.st.InsertRequestLog(ctx, &store.RequestLog{ProjectID: e.proj.ID, ModelName: "m", StatusCode: 200,
		SessionID: "run-1", ToolNames: []string{"get_weather"}, ToolsDenied: 1}); err != nil {
		t.Fatal(err)
	}
	viewer := e.user("viewer", "viewer", e.proj.ID)
	admin := e.user("root", "admin")
	outsider := e.user("outsider", "viewer", e.other.ID)
	pid := itoa(e.proj.ID)

	// Any member reads, newest activity first, with the totals.
	status, list := e.doList("/api/projects/"+pid+"/sessions", viewer.token)
	if status != http.StatusOK || len(list) != 2 {
		t.Fatalf("list: %d %v", status, list)
	}
	first, _ := list[0].(map[string]any)
	if first["session_id"] != "run-2" {
		t.Errorf("first session = %v, want the most recently active one, run-2", first["session_id"])
	}
	second, _ := list[1].(map[string]any)
	if second["session_id"] != "run-1" || second["requests"] != float64(2) || second["tokens"] != float64(30) {
		t.Errorf("run-1 = %v", second)
	}

	status, detail := e.do(http.MethodGet, "/api/projects/"+pid+"/sessions/run-1", nil, viewer.token)
	sess, _ := detail["session"].(map[string]any)
	if status != http.StatusOK || sess["session_id"] != "run-1" || sess["tokens"] != float64(30) {
		t.Fatalf("detail: %d %v", status, detail)
	}
	rows, _ := detail["requests"].([]any)
	if len(rows) != 1 {
		t.Fatalf("requests = %v", detail["requests"])
	}
	row, _ := rows[0].(map[string]any)
	if row["session_id"] != "run-1" || row["tools_denied"] != float64(1) || !reflect.DeepEqual(row["tool_names"], []any{"get_weather"}) {
		t.Errorf("request row = %v", row)
	}

	// Admins see every project; a non-member does not see this one.
	if status, _ := e.doList("/api/projects/"+pid+"/sessions", admin.token); status != http.StatusOK {
		t.Errorf("admin list: %d", status)
	}
	if status, _ := e.doList("/api/projects/"+pid+"/sessions", outsider.token); status != http.StatusNotFound {
		t.Errorf("non-member list: %d, want 404", status)
	}
	if status, _ := e.do(http.MethodGet, "/api/projects/"+pid+"/sessions/run-1", nil, outsider.token); status != http.StatusNotFound {
		t.Errorf("non-member detail: %d, want 404", status)
	}
	// A session of another project is not reachable through this one.
	if status, _ := e.do(http.MethodGet, "/api/projects/"+pid+"/sessions/secret-run", nil, viewer.token); status != http.StatusNotFound {
		t.Errorf("another project's session: %d, want 404", status)
	}
	if status, _ := e.do(http.MethodGet, "/api/projects/"+pid+"/sessions/nope", nil, viewer.token); status != http.StatusNotFound {
		t.Errorf("unknown session: %d, want 404", status)
	}
	if status, _ := e.do(http.MethodGet, "/api/projects/"+pid+"/sessions/"+strings.Repeat("a", 129), nil, viewer.token); status != http.StatusNotFound {
		t.Errorf("an id that cannot be a session: %d, want 404", status)
	}
	if status, _ := e.doList("/api/projects/"+pid+"/sessions", "not-a-token"); status != http.StatusUnauthorized {
		t.Errorf("no session: %d, want 401", status)
	}
}
