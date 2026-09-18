package rag

import (
	"encoding/json"
	"testing"

	"github.com/ragmux/ragmux/internal/provider"
)

func TestInjectContext(t *testing.T) {
	msgs := []provider.Message{
		{Role: "user", Content: provider.TextContent("q")},
	}
	out := InjectContext(msgs, "CTX")
	if len(out) != 2 || out[0].Role != "system" || out[0].Text() != "CTX" {
		t.Errorf("no system: %+v", out)
	}
	if len(msgs) != 1 {
		t.Errorf("input mutated")
	}
	msgs = []provider.Message{
		{Role: "system", Content: provider.TextContent("be nice")},
		{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"q"}]`)},
	}
	out = InjectContext(msgs, "CTX")
	if len(out) != 2 || out[0].Text() != "CTX\n\nbe nice" {
		t.Errorf("system not extended: %q", out[0].Text())
	}
	if LastUserQuery(out) != "q" {
		t.Errorf("last user query = %q", LastUserQuery(out))
	}
}
