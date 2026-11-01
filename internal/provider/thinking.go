package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// Thinking block types, spelled as Anthropic spells them. They are the shape
// of ragmux_thinking for every provider (ADR-031).
const (
	thinkingBlockThinking = "thinking"
	thinkingBlockRedacted = "redacted_thinking"
)

// invalidThinkingCode is the error code of a malformed ragmux_thinking.
const invalidThinkingCode = "invalid_ragmux_thinking"

// ThinkingBlock is one element of ragmux_thinking.
//
// In a complete message (a non-streaming response, or a request replaying an
// earlier turn) a block is whole: type "thinking" carries Thinking and
// Signature, type "redacted_thinking" carries Data. In a stream the same
// struct is a partial update, and Index says which block it extends: the
// client concatenates Thinking and sets Signature on the block with that
// Index, the way it accumulates tool_calls deltas.
//
// A thinking block whose text is empty is complete when it has a signature
// (Claude 5 returns thinking text omitted), which is why the empty fields are
// not required on input.
type ThinkingBlock struct {
	Index     *int   `json:"index,omitempty"`
	Type      string `json:"type"`
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
	Data      string `json:"data,omitempty"`
}

// ThinkingField renders blocks as the raw value of Message.Thinking.
func ThinkingField(blocks []ThinkingBlock) json.RawMessage {
	if len(blocks) == 0 {
		return nil
	}
	b, err := json.Marshal(blocks)
	if err != nil {
		return nil
	}
	return b
}

func invalidThinking(format string, args ...any) *Error {
	return &Error{Status: http.StatusBadRequest, Type: "invalid_request_error", Code: invalidThinkingCode,
		Message: "ragmux_thinking " + fmt.Sprintf(format, args...)}
}

// thinkingPresent reports whether a raw field holds a value: absent and JSON
// null both mean the client sent nothing.
func thinkingPresent(raw json.RawMessage) bool {
	n := len(raw)
	if n == 0 {
		return false
	}
	return string(raw) != "null"
}

// parseThinkingBlocks decodes and validates one message's ragmux_thinking.
// Only the shape is checked: the value is an array; each element is an object
// whose type is "thinking" or "redacted_thinking"; thinking and signature, if
// present, are strings; data is a non-empty string. The signature cannot be
// verified here and is not: that is the upstream's call.
func parseThinkingBlocks(raw json.RawMessage) ([]ThinkingBlock, error) {
	if !thinkingPresent(raw) {
		return nil, nil
	}
	var elems []json.RawMessage
	if err := json.Unmarshal(raw, &elems); err != nil {
		return nil, invalidThinking("must be an array of thinking blocks")
	}
	out := make([]ThinkingBlock, 0, len(elems))
	for i, e := range elems {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(e, &obj); err != nil || obj == nil {
			return nil, invalidThinking("[%d] must be an object", i)
		}
		str := func(key string) (string, bool, error) {
			v, ok := obj[key]
			if !ok {
				return "", false, nil
			}
			var s string
			if err := json.Unmarshal(v, &s); err != nil {
				return "", true, invalidThinking("[%d].%s must be a string", i, key)
			}
			return s, true, nil
		}
		typ, _, err := str("type")
		if err != nil {
			return nil, err
		}
		b := ThinkingBlock{Type: typ}
		switch typ {
		case thinkingBlockThinking:
			if b.Thinking, _, err = str("thinking"); err != nil {
				return nil, err
			}
			if b.Signature, _, err = str("signature"); err != nil {
				return nil, err
			}
		case thinkingBlockRedacted:
			var ok bool
			if b.Data, ok, err = str("data"); err != nil {
				return nil, err
			}
			if !ok || b.Data == "" {
				return nil, invalidThinking("[%d].data must be a non-empty string", i)
			}
		default:
			return nil, invalidThinking("[%d].type must be %q or %q", i, thinkingBlockThinking, thinkingBlockRedacted)
		}
		out = append(out, b)
	}
	return out, nil
}

// ValidateRequestThinking checks the ragmux_thinking of every message of a
// request: a malformed value, or one on a message that is not an assistant
// turn, is a 400 with code invalid_ragmux_thinking (ADR-031). Every adapter
// calls it, so the answer does not depend on which connection a request lands
// on; a gateway may call it earlier to fail before routing.
func ValidateRequestThinking(msgs []Message) error {
	for i, m := range msgs {
		if !thinkingPresent(m.Thinking) {
			continue
		}
		if m.Role != "assistant" {
			return invalidThinking("is only allowed on assistant messages (messages[%d] has role %q)", i, m.Role)
		}
		if _, err := parseThinkingBlocks(m.Thinking); err != nil {
			if pe, ok := err.(*Error); ok {
				pe.Message = fmt.Sprintf("messages[%d]: %s", i, pe.Message)
			}
			return err
		}
	}
	return nil
}

// replayableThinking is what the Anthropic adapter may write back: blocks it
// can sign for. A thinking block without a signature came from a provider
// whose thinking Anthropic cannot read (Gemini, Ollama, a compat upstream),
// and a redacted block without data carries nothing.
func replayableThinking(blocks []ThinkingBlock) []ThinkingBlock {
	var out []ThinkingBlock
	for _, b := range blocks {
		switch {
		case b.Type == thinkingBlockThinking && b.Signature != "":
			out = append(out, b)
		case b.Type == thinkingBlockRedacted && b.Data != "":
			out = append(out, b)
		}
	}
	return out
}

// hasReplayableThinking reports whether any message carries a block the
// Anthropic adapter would send back. The messages were validated before.
func hasReplayableThinking(msgs []Message) bool {
	for _, m := range msgs {
		if m.Role != "assistant" {
			continue
		}
		blocks, err := parseThinkingBlocks(m.Thinking)
		if err == nil && len(replayableThinking(blocks)) > 0 {
			return true
		}
	}
	return false
}

// unsignedThinkingText joins the text of a message's unsigned thinking blocks:
// the thinking an Ollama model produced itself, which Ollama takes back in
// message.thinking. Signed blocks are Anthropic's and mean nothing elsewhere.
func unsignedThinkingText(raw json.RawMessage) string {
	blocks, err := parseThinkingBlocks(raw)
	if err != nil {
		return ""
	}
	var s string
	for _, b := range blocks {
		if b.Type == thinkingBlockThinking && b.Signature == "" {
			s += b.Thinking
		}
	}
	return s
}

// hasReasoningContent reports whether any message carries a non-empty
// reasoning_content.
func hasReasoningContent(msgs []Message) bool {
	for _, m := range msgs {
		if !thinkingPresent(m.ReasoningContent) {
			continue
		}
		var s string
		if json.Unmarshal(m.ReasoningContent, &s) == nil && s == "" {
			continue
		}
		return true
	}
	return false
}

// reasoningContentDropped is the Warning text for a reasoning_content an
// adapter that translates the request had to leave out.
func reasoningContentDropped(providerType string) string {
	return "reasoning_content is only forwarded to OpenAI-compatible connections; dropped for " + providerType
}

// foldReasoningContent turns an OpenAI-compatible reply's reasoning_content
// into an unsigned ragmux_thinking block, so a client can read thinking from
// one field whichever provider answered. reasoning_content itself stays as
// the upstream sent it. An upstream that already sent ragmux_thinking (another
// gateway) is left alone.
func foldReasoningContent(resp *ChatResponse) {
	for i := range resp.Choices {
		m := &resp.Choices[i].Message
		if len(m.Thinking) > 0 || m.ReasoningContent == nil || *m.ReasoningContent == "" {
			continue
		}
		m.Thinking = []ThinkingBlock{{Type: thinkingBlockThinking, Thinking: *m.ReasoningContent}}
	}
}

// foldReasoningDelta is foldReasoningContent for a stream chunk. Every
// reasoning_content fragment becomes a fragment of block 0.
func foldReasoningDelta(chunk *StreamChunk) {
	zero := 0
	for i := range chunk.Choices {
		d := &chunk.Choices[i].Delta
		if len(d.Thinking) > 0 || d.ReasoningContent == nil || *d.ReasoningContent == "" {
			continue
		}
		d.Thinking = []ThinkingBlock{{Index: &zero, Type: thinkingBlockThinking, Thinking: *d.ReasoningContent}}
	}
}

// thinkingBlocksOf wraps one piece of unsigned thinking text as a whole block
// (non-streaming).
func thinkingBlocksOf(text string) []ThinkingBlock {
	if text == "" {
		return nil
	}
	return []ThinkingBlock{{Type: thinkingBlockThinking, Thinking: text}}
}

// thinkingDeltaOf wraps one fragment of unsigned thinking text as a partial
// update of block 0 (streaming).
func thinkingDeltaOf(text string) []ThinkingBlock {
	if text == "" {
		return nil
	}
	zero := 0
	return []ThinkingBlock{{Index: &zero, Type: thinkingBlockThinking, Thinking: text}}
}
