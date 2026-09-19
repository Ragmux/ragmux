package provider

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestToolCallStreamIndexes(t *testing.T) {
	var s toolCallStream
	// Two blocks opened before either finishes: the index follows the block,
	// not the order the argument deltas arrive in.
	if got := string(s.Open(3, "id_a", "a")); got != `[{"function":{"arguments":"","name":"a"},"id":"id_a","index":0,"type":"function"}]` {
		t.Errorf("open a = %s", got)
	}
	if got := string(s.Open(5, "id_b", "b")); got != `[{"function":{"arguments":"","name":"b"},"id":"id_b","index":1,"type":"function"}]` {
		t.Errorf("open b = %s", got)
	}
	if got := string(s.Args(5, `{"x":`)); got != `[{"function":{"arguments":"{\"x\":"},"index":1}]` {
		t.Errorf("args b = %s", got)
	}
	if got := string(s.Args(3, "{}")); got != `[{"function":{"arguments":"{}"},"index":0}]` {
		t.Errorf("args a = %s", got)
	}
	if got := s.Args(9, "{}"); got != nil {
		t.Errorf("args for an unknown block = %s", got)
	}
	// Whole shares the counter, so a provider that mixes both framings still
	// numbers its calls consecutively.
	var whole []struct {
		Index    int    `json:"index"`
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	if err := json.Unmarshal(s.Whole("", "c", `{"k":1}`), &whole); err != nil {
		t.Fatal(err)
	}
	if len(whole) != 1 || whole[0].Index != 2 || whole[0].Type != "function" || whole[0].Function.Name != "c" ||
		whole[0].Function.Arguments != `{"k":1}` || !strings.HasPrefix(whole[0].ID, "call_") {
		t.Errorf("whole = %+v", whole)
	}
	if err := json.Unmarshal(s.Whole("call_x", "d", "not json"), &whole); err != nil {
		t.Fatal(err)
	}
	// An unparseable argument string is replaced, and a provided id kept.
	if whole[0].Index != 3 || whole[0].ID != "call_x" || whole[0].Function.Arguments != "{}" {
		t.Errorf("whole = %+v", whole)
	}
	if s.Len() != 4 {
		t.Errorf("len = %d", s.Len())
	}
}

func TestToolCallsJSON(t *testing.T) {
	if got := toolCallsJSON(nil); got != nil {
		t.Errorf("empty = %s", got)
	}
	got := string(toolCallsJSON([]toolCall{
		{ID: "call_1", Name: "a", Arguments: `{"city":"Ankara"}`},
		{Name: "b"},
	}))
	if !strings.Contains(got, `{"arguments":"{\"city\":\"Ankara\"}","name":"a"}`) || strings.Contains(got, `"index"`) {
		t.Errorf("calls = %s", got)
	}
	// An empty or invalid argument string becomes an empty object, and a
	// missing id is generated.
	if !strings.Contains(got, `{"arguments":"{}","name":"b"}`) || strings.Count(got, `"id":"call_`) != 2 {
		t.Errorf("calls = %s", got)
	}
	if bad := string(toolCallsJSON([]toolCall{{ID: "x", Name: "a", Arguments: "{oops"}})); !strings.Contains(bad, `"arguments":"{}"`) {
		t.Errorf("invalid arguments = %s", bad)
	}
}
