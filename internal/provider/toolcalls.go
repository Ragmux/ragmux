package provider

import "encoding/json"

// Tool calls stay json.RawMessage end to end, but the OpenAI streaming shape
// they have to end up in is the same for every provider: an array of deltas
// carrying an "index" clients accumulate by. This file owns that shape so an
// adapter only has to say what it received, not how to frame it.

// noBlock is the block key for providers that deliver a tool call whole
// instead of streaming its arguments, so there is nothing to correlate later.
const noBlock = -1

// toolCallStream maps an upstream block (Anthropic's content-block index) to
// the OpenAI tool_calls index and hands out the deltas. One value per
// response: the index must keep counting across the whole stream, not restart
// inside each upstream frame, or clients merge two calls into one.
type toolCallStream struct {
	index map[int]int
	next  int
}

// Open starts a tool call whose arguments arrive later, in Args deltas.
func (t *toolCallStream) Open(block int, id, name string) json.RawMessage {
	n := t.assign(block)
	b, _ := json.Marshal([]map[string]any{{
		"index": n, "id": callID(id), "type": "function",
		"function": map[string]string{"name": name, "arguments": ""},
	}})
	return b
}

// Args is one argument fragment of an open call. An unknown block returns nil:
// upstream frames that were never opened carry nothing a client could place.
func (t *toolCallStream) Args(block int, partial string) json.RawMessage {
	n, ok := t.index[block]
	if !ok {
		return nil
	}
	b, _ := json.Marshal([]map[string]any{{
		"index": n, "function": map[string]string{"arguments": partial},
	}})
	return b
}

// Whole is a complete call in a single delta, for providers that never split
// arguments (Gemini, Ollama).
func (t *toolCallStream) Whole(id, name, args string) json.RawMessage {
	n := t.assign(noBlock)
	c := toolCall{ID: id, Name: name, Arguments: args}.normalised()
	b, _ := json.Marshal([]map[string]any{{
		"index": n, "id": c.ID, "type": "function",
		"function": map[string]string{"name": c.Name, "arguments": c.Arguments},
	}})
	return b
}

// Len is how many calls the stream has emitted, which is how an adapter knows
// the finish reason is tool_calls even when the upstream does not say so.
func (t *toolCallStream) Len() int { return t.next }

func (t *toolCallStream) assign(block int) int {
	n := t.next
	t.next++
	if block != noBlock {
		if t.index == nil {
			t.index = map[int]int{}
		}
		t.index[block] = n
	}
	return n
}

// toolCall is one finished call, as an adapter read it off the wire.
type toolCall struct {
	ID        string
	Name      string
	Arguments string
}

// normalised fills in what OpenAI clients require but providers omit: an id
// (Gemini and Ollama correlate by name and send none) and arguments that are
// at least parseable JSON.
func (c toolCall) normalised() toolCall {
	c.ID = callID(c.ID)
	if c.Arguments == "" || !json.Valid([]byte(c.Arguments)) {
		c.Arguments = "{}"
	}
	return c
}

func callID(id string) string {
	if id != "" {
		return id
	}
	return "call_" + randomID(24)
}

// toolCallsJSON renders finished calls in the non-streaming shape, which
// carries no "index" (the array position is the index).
func toolCallsJSON(calls []toolCall) json.RawMessage {
	if len(calls) == 0 {
		return nil
	}
	out := make([]map[string]any, len(calls))
	for i, c := range calls {
		c = c.normalised()
		out[i] = map[string]any{
			"id": c.ID, "type": "function",
			"function": map[string]string{"name": c.Name, "arguments": c.Arguments},
		}
	}
	b, _ := json.Marshal(out)
	return b
}
