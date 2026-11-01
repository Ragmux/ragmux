package provider

import (
	"encoding/json"
	"net/http"
	"strings"
)

// A canonical answer written as a Messages answer (ADR-028): the JSON body, the
// SSE event stream, and the error envelope.

// MessagesStopReason maps an OpenAI finish_reason to a Messages stop_reason.
// stop_sequence stays null in the answer: which sequence matched is not known
// on this path.
func MessagesStopReason(finish string) string {
	switch finish {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "content_filter":
		return "refusal"
	}
	return "end_turn"
}

// messagesBlock is a content block of a Messages answer.
type messagesBlock struct {
	Type      string          `json:"type"`
	Text      *string         `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	Thinking  *string         `json:"thinking,omitempty"`
	Signature *string         `json:"signature,omitempty"`
	Data      string          `json:"data,omitempty"`
}

// messagesUsage is the usage object of an answer. It is AnthropicUsageBlock:
// the same four counters the adapter reads from the API.
type messagesUsage = AnthropicUsageBlock

// usageBlockOf maps the canonical usage onto the Messages counters: the cached
// part of prompt_tokens is reported apart from input_tokens, the way the API
// reports it.
func usageBlockOf(u *Usage) messagesUsage {
	if u == nil {
		return messagesUsage{}
	}
	cached, written := u.CachedTokens(), u.CacheWriteTokens()
	in := max(u.PromptTokens-cached-written, 0)
	return messagesUsage{InputTokens: in, OutputTokens: u.CompletionTokens,
		CacheCreationInputTokens: written, CacheReadInputTokens: cached}
}

// messagesID turns an upstream completion id into a message id. An id that
// already looks like one is kept.
func messagesID(id string) string {
	switch {
	case strings.HasPrefix(id, "msg_"):
		return id
	case id == "":
		return "msg_" + randomID()
	}
	return "msg_" + strings.TrimPrefix(id, "chatcmpl-")
}

// MessagesResponse is a Messages answer.
type MessagesResponse struct {
	ID           string          `json:"id"`
	Type         string          `json:"type"`
	Role         string          `json:"role"`
	Model        string          `json:"model"`
	Content      []messagesBlock `json:"content"`
	StopReason   string          `json:"stop_reason"`
	StopSequence *string         `json:"stop_sequence"`
	Usage        messagesUsage   `json:"usage"`
	// Ragmux is the object the client asked to get back (ragmux.sources).
	Ragmux json.RawMessage `json:"ragmux,omitempty"`
}

// ChatToMessages writes a canonical answer as a Messages answer. model is the
// string the client asked for, echoed as ADR-018 sets out.
func ChatToMessages(resp *ChatResponse, model string, inline []byte) MessagesResponse {
	out := MessagesResponse{ID: messagesID(resp.ID), Type: "message", Role: "assistant", Model: model,
		Content: []messagesBlock{}, StopReason: "end_turn", Usage: usageBlockOf(resp.Usage), Ragmux: inline}
	if len(resp.Choices) == 0 {
		return out
	}
	ch := resp.Choices[0]
	for _, tb := range ch.Message.Thinking {
		switch tb.Type {
		case thinkingBlockThinking:
			text, sig := tb.Thinking, tb.Signature
			out.Content = append(out.Content, messagesBlock{Type: thinkingBlockThinking, Thinking: &text, Signature: &sig})
		case thinkingBlockRedacted:
			out.Content = append(out.Content, messagesBlock{Type: thinkingBlockRedacted, Data: tb.Data})
		}
	}
	var text string
	switch {
	case ch.Message.Content != nil && *ch.Message.Content != "":
		text = *ch.Message.Content
	case ch.Message.Refusal != nil:
		text = *ch.Message.Refusal
	}
	if text != "" {
		out.Content = append(out.Content, messagesBlock{Type: "text", Text: &text})
	}
	var calls []struct {
		ID       string `json:"id"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	if len(ch.Message.ToolCalls) > 0 {
		_ = json.Unmarshal(ch.Message.ToolCalls, &calls)
	}
	for _, c := range calls {
		input := json.RawMessage(c.Function.Arguments)
		if !json.Valid(input) || len(input) == 0 {
			input = json.RawMessage("{}")
		}
		id := c.ID
		if id == "" {
			id = "toolu_" + randomID()
		}
		out.Content = append(out.Content, messagesBlock{Type: "tool_use", ID: id, Name: c.Function.Name, Input: input})
	}
	if len(out.Content) == 0 {
		empty := ""
		out.Content = append(out.Content, messagesBlock{Type: "text", Text: &empty})
	}
	if ch.FinishReason != nil {
		out.StopReason = MessagesStopReason(*ch.FinishReason)
	}
	return out
}

// MessagesErrorType is the Messages error type of an HTTP status (ADR-028).
func MessagesErrorType(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusServiceUnavailable:
		return "overloaded_error"
	case http.StatusGatewayTimeout:
		return "timeout_error"
	}
	if status >= 500 {
		return "api_error"
	}
	return "invalid_request_error"
}

// MessagesError renders the error envelope: Anthropic's, with the gateway's
// own code in error.code when it has one and the request id beside it.
func MessagesError(status int, code, message, requestID string) []byte {
	type errBody struct {
		Type    string `json:"type"`
		Message string `json:"message"`
		Code    string `json:"code,omitempty"`
	}
	b, _ := marshalNoEscape(struct {
		Type      string  `json:"type"`
		Error     errBody `json:"error"`
		RequestID string  `json:"request_id,omitempty"`
	}{"error", errBody{MessagesErrorType(status), message, code}, requestID})
	return b
}

// MessagesEncoder writes a canonical stream as the Messages event sequence:
// message_start, content blocks, message_delta, message_stop. The canonical
// chunks carry usage and the finish reason in separate trailing chunks, so the
// closing message_delta waits for Finish.
type MessagesEncoder struct {
	model  string
	id     string
	inline []byte

	started bool
	next    int // index of the next block
	open    int // index of the open block, -1 when none
	kind    string
	// thinking and tools map the index a canonical chunk names to the block
	// that was opened for it. A block that has been closed is not reopened.
	thinking map[int]int
	tools    map[int]int
	sawTool  bool
	stop     string
	usage    *Usage
}

// NewMessagesEncoder starts the event sequence of one answer. model is echoed;
// inline is the "ragmux" object for message_start, or nil.
func NewMessagesEncoder(model string, inline []byte) *MessagesEncoder {
	return &MessagesEncoder{model: model, id: "msg_" + randomID(), inline: inline, open: -1,
		thinking: map[int]int{}, tools: map[int]int{}}
}

// sse renders one event.
func sse(event string, v any) []byte {
	b, _ := marshalNoEscape(v)
	out := make([]byte, 0, len(b)+len(event)+16)
	out = append(out, "event: "...)
	out = append(out, event...)
	out = append(out, "\ndata: "...)
	out = append(out, b...)
	return append(out, '\n', '\n')
}

// MessagesPing is the keep-alive event written while the upstream is quiet.
func MessagesPing() []byte {
	return []byte("event: ping\ndata: {\"type\": \"ping\"}\n\n")
}

func (e *MessagesEncoder) begin(out *[]byte) {
	if e.started {
		return
	}
	e.started = true
	msg := map[string]any{"id": e.id, "type": "message", "role": "assistant", "model": e.model,
		"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
		"usage": map[string]int{"input_tokens": 0, "output_tokens": 0}}
	if len(e.inline) > 0 {
		msg["ragmux"] = json.RawMessage(e.inline)
	}
	*out = append(*out, sse("message_start", map[string]any{"type": "message_start", "message": msg})...)
}

func (e *MessagesEncoder) closeOpen(out *[]byte) {
	if e.open < 0 {
		return
	}
	*out = append(*out, sse("content_block_stop", map[string]any{"type": "content_block_stop", "index": e.open})...)
	e.open, e.kind = -1, ""
}

func (e *MessagesEncoder) openBlock(out *[]byte, kind string, block any) int {
	e.closeOpen(out)
	idx := e.next
	e.next++
	*out = append(*out, sse("content_block_start", map[string]any{"type": "content_block_start", "index": idx, "content_block": block})...)
	e.open, e.kind = idx, kind
	return idx
}

func (e *MessagesEncoder) delta(out *[]byte, idx int, d map[string]any) {
	*out = append(*out, sse("content_block_delta", map[string]any{"type": "content_block_delta", "index": idx, "delta": d})...)
}

// Chunk encodes one canonical chunk; the result may be empty.
func (e *MessagesEncoder) Chunk(c StreamChunk) []byte {
	var out []byte
	if c.Usage != nil {
		e.usage = c.Usage
	}
	for _, ch := range c.Choices {
		if ch.Index != 0 {
			continue
		}
		e.begin(&out)
		d := ch.Delta
		for _, tb := range d.Thinking {
			e.thinkingUpdate(&out, tb)
		}
		if d.Content != nil && *d.Content != "" {
			if e.kind != "text" {
				e.openBlock(&out, "text", map[string]any{"type": "text", "text": ""})
			}
			e.delta(&out, e.open, map[string]any{"type": "text_delta", "text": *d.Content})
		}
		if len(d.ToolCalls) > 0 {
			e.toolUpdate(&out, d.ToolCalls)
		}
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			e.closeOpen(&out)
			e.stop = MessagesStopReason(*ch.FinishReason)
		}
	}
	return out
}

func (e *MessagesEncoder) thinkingUpdate(out *[]byte, tb ThinkingBlock) {
	key := 0
	if tb.Index != nil {
		key = *tb.Index
	}
	switch tb.Type {
	case thinkingBlockRedacted:
		e.openBlock(out, thinkingBlockRedacted, map[string]any{"type": thinkingBlockRedacted, "data": tb.Data})
		e.closeOpen(out)
	case thinkingBlockThinking:
		idx, ok := e.thinking[key]
		if !ok {
			idx = e.openBlock(out, thinkingBlockThinking, map[string]any{"type": thinkingBlockThinking, "thinking": "", "signature": ""})
			e.thinking[key] = idx
		}
		if idx != e.open {
			return // its block was closed by a later one
		}
		if tb.Thinking != "" {
			e.delta(out, idx, map[string]any{"type": "thinking_delta", "thinking": tb.Thinking})
		}
		if tb.Signature != "" {
			e.delta(out, idx, map[string]any{"type": "signature_delta", "signature": tb.Signature})
		}
	}
}

func (e *MessagesEncoder) toolUpdate(out *[]byte, raw json.RawMessage) {
	var calls []struct {
		Index    int    `json:"index"`
		ID       string `json:"id"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	if json.Unmarshal(raw, &calls) != nil {
		return
	}
	for _, tc := range calls {
		idx, ok := e.tools[tc.Index]
		if !ok {
			id := tc.ID
			if id == "" {
				id = "toolu_" + randomID()
			}
			idx = e.openBlock(out, "tool_use", map[string]any{"type": "tool_use", "id": id, "name": tc.Function.Name, "input": map[string]any{}})
			e.tools[tc.Index] = idx
			e.sawTool = true
		}
		if tc.Function.Arguments != "" && idx == e.open {
			e.delta(out, idx, map[string]any{"type": "input_json_delta", "partial_json": tc.Function.Arguments})
		}
	}
}

// Finish closes the answer: the open block, then message_delta with the stop
// reason and the usage, then message_stop.
func (e *MessagesEncoder) Finish() []byte {
	var out []byte
	e.begin(&out)
	e.closeOpen(&out)
	stop := e.stop
	if stop == "" {
		stop = "end_turn"
		if e.sawTool {
			stop = "tool_use"
		}
	}
	out = append(out, sse("message_delta", map[string]any{"type": "message_delta",
		"delta": map[string]any{"stop_reason": stop, "stop_sequence": nil},
		"usage": usageBlockOf(e.usage)})...)
	return append(out, sse("message_stop", map[string]any{"type": "message_stop"})...)
}

// Error is the event that ends a stream that failed after it began.
func (e *MessagesEncoder) Error(status int, message string) []byte {
	return MessagesErrorEvent(status, message)
}

// MessagesErrorEvent is the error event of a Messages stream (ADR-028).
func MessagesErrorEvent(status int, message string) []byte {
	return sse("error", map[string]any{"type": "error",
		"error": map[string]string{"type": MessagesErrorType(status), "message": message}})
}
