package provider

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

func TestExtraPolicyPermits(t *testing.T) {
	cases := []struct {
		name        string
		allow, deny []string
		field       string
		want        bool
	}{
		{"no lists permit everything", nil, nil, "store", true},
		{"deny exact", nil, []string{"store"}, "store", false},
		{"deny leaves the others", nil, []string{"store"}, "seed", true},
		{"deny glob", nil, []string{"web_*"}, "web_search_options", false},
		{"deny glob does not match a shorter name", nil, []string{"web_*"}, "web", true},
		{"deny star denies all", nil, []string{"*"}, "seed", false},
		{"allow list permits a match", []string{"seed"}, nil, "seed", true},
		{"allow list refuses the rest", []string{"seed"}, nil, "logprobs", false},
		{"allow glob", []string{"guided_*"}, nil, "guided_json", true},
		{"deny wins over allow", []string{"*"}, []string{"store"}, "store", false},
		{"deny wins over an exact allow", []string{"store"}, []string{"sto*"}, "store", false},
		{"matching is case sensitive", nil, []string{"store"}, "Store", true},
		{"the only wildcard is star", nil, []string{"s.ore"}, "store", true},
	}
	for _, tc := range cases {
		p := ExtraPolicy{Allow: tc.allow, Deny: tc.deny}
		if got := p.Permits(tc.field); got != tc.want {
			t.Errorf("%s: Permits(%q) = %v, want %v", tc.name, tc.field, got, tc.want)
		}
	}
	if !(ExtraPolicy{}).Empty() || (ExtraPolicy{Deny: []string{"a"}}).Empty() || (ExtraPolicy{Allow: []string{"a"}}).Empty() {
		t.Error("Empty is wrong")
	}
}

func TestStripExtra(t *testing.T) {
	extra := func() map[string]json.RawMessage {
		return map[string]json.RawMessage{
			"seed": json.RawMessage("7"), "logprobs": json.RawMessage("true"), "parallel_tool_calls": json.RawMessage("false"),
			"reasoning_effort": json.RawMessage(`"low"`), "store": json.RawMessage("true"), "service_tier": json.RawMessage(`"flex"`),
		}
	}

	// Empty lists change nothing and report nothing.
	e := extra()
	if got := (ExtraPolicy{}).StripExtra(e); got != nil || !reflect.DeepEqual(e, extra()) {
		t.Errorf("empty policy removed %v, left %v", got, e)
	}

	// Deny: the names come back sorted and are gone from the map.
	e = extra()
	got := ExtraPolicy{Deny: []string{"store", "service_*"}}.StripExtra(e)
	if !reflect.DeepEqual(got, []string{"service_tier", "store"}) {
		t.Errorf("removed = %v", got)
	}
	if _, has := e["store"]; has || len(e) != 4 {
		t.Errorf("left = %v", e)
	}

	// Allow: what it does not match goes.
	e = extra()
	got = ExtraPolicy{Allow: []string{"seed", "reasoning_effort"}}.StripExtra(e)
	if len(got) != 4 || len(e) != 2 || e["seed"] == nil || e["reasoning_effort"] == nil {
		t.Errorf("removed %v, left %v", got, e)
	}

	// Nothing to strip from a nil map.
	if got := (ExtraPolicy{Deny: []string{"*"}}).StripExtra(nil); got != nil {
		t.Errorf("nil map: %v", got)
	}
}

// Known fields are not in Extra, so no list can reach them, and the request
// that goes upstream keeps them whatever the policy says.
func TestStripExtraLeavesKnownFields(t *testing.T) {
	var req ChatRequest
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"temperature":0.5,"stream":false,"seed":3,"store":true}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	removed := ExtraPolicy{Deny: []string{"*"}}.StripExtra(req.Extra)
	if !reflect.DeepEqual(removed, []string{"seed", "store"}) {
		t.Fatalf("removed = %v", removed)
	}
	out, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	_ = json.Unmarshal(out, &wire)
	if wire["model"] != "m" || wire["temperature"] != 0.5 || wire["messages"] == nil {
		t.Errorf("known fields changed: %s", out)
	}
	if _, has := wire["seed"]; has {
		t.Errorf("seed went upstream: %s", out)
	}
	if _, has := wire["store"]; has {
		t.Errorf("store went upstream: %s", out)
	}
}

func TestStripMessagesFields(t *testing.T) {
	const raw = `{"model":"claude-opus-5-5","max_tokens":64,"top_k":4,"mcp_servers":[{"type":"url","url":"https://x"}],` +
		`"system":"s","messages":[{"role":"user","content":"hi"}],"container":"c1","context_management":{"edits":[]},` +
		`"thinking":{"type":"adaptive"},"metadata":{"user_id":"u"},"tools":[],"tool_choice":{"type":"auto"},"output_config":{"effort":"low"},` +
		`"stop_sequences":["x"],"temperature":0.2,"top_p":0.9,"stream":false}`
	parse := func() *MessagesBody {
		b, err := ParseMessagesBody([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	// Empty lists: the bytes are the ones that came in.
	b := parse()
	if got := (ExtraPolicy{}).StripMessagesFields(b); got != nil || !bytes.Equal(b.Bytes(), parse().Bytes()) {
		t.Errorf("empty policy removed %v", got)
	}

	// Deny: only fields outside handledMessagesFields are candidates, and the
	// rest keep their order.
	b = parse()
	got := ExtraPolicy{Deny: []string{"mcp_servers", "container"}}.StripMessagesFields(b)
	if !reflect.DeepEqual(got, []string{"container", "mcp_servers"}) {
		t.Errorf("removed = %v", got)
	}
	for _, k := range []string{"mcp_servers", "container"} {
		if b.Has(k) {
			t.Errorf("%s is still in the body", k)
		}
	}
	for _, k := range []string{"top_k", "context_management", "model", "messages", "thinking", "metadata"} {
		if !b.Has(k) {
			t.Errorf("%s was removed", k)
		}
	}

	// Deny star and an allow list that names nothing: the handled fields are
	// still not touched, whatever the lists say.
	for name, p := range map[string]ExtraPolicy{"deny *": {Deny: []string{"*"}}, "allow nothing useful": {Allow: []string{"zzz"}}} {
		b = parse()
		got = p.StripMessagesFields(b)
		if !reflect.DeepEqual(got, []string{"container", "context_management", "mcp_servers", "top_k"}) {
			t.Errorf("%s: removed = %v", name, got)
		}
		for k := range handledMessagesFields {
			if raw := parse(); raw.Has(k) && !b.Has(k) {
				t.Errorf("%s: handled field %s was removed", name, k)
			}
		}
	}

	// Allow keeps what it names.
	b = parse()
	got = ExtraPolicy{Allow: []string{"top_k", "mcp_*"}}.StripMessagesFields(b)
	if !reflect.DeepEqual(got, []string{"container", "context_management"}) || !b.Has("top_k") || !b.Has("mcp_servers") {
		t.Errorf("allow: removed %v", got)
	}
}

// A key the client repeats, or writes with an escape, is one field once the
// body is parsed, and the policy sees it under that name.
func TestStripMessagesFieldsRepeatedAndEscapedKeys(t *testing.T) {
	b, err := ParseMessagesBody([]byte(`{"model":"m","messages":[],"mcp_servers":[1],"mcp_servers":[2],"container":"c"}`))
	if err != nil {
		t.Fatal(err)
	}
	got := ExtraPolicy{Deny: []string{"mcp_servers", "container"}}.StripMessagesFields(b)
	if !reflect.DeepEqual(got, []string{"container", "mcp_servers"}) {
		t.Errorf("removed = %v", got)
	}
	if bytes.Contains(b.Bytes(), []byte("mcp")) || bytes.Contains(b.Bytes(), []byte("container")) {
		t.Errorf("body = %s", b.Bytes())
	}
}
