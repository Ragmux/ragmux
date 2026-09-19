// Package provider translates OpenAI-format chat requests into each upstream
// provider's native API and normalises responses (JSON and SSE) back into the
// OpenAI schema.
package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Message is one OpenAI chat message. Content is kept raw because it may be
// a string or an array of content parts.
type Message struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	Name       string          `json:"name,omitempty"`
	ToolCalls  json.RawMessage `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

// Text extracts plain text from a string or parts-array content.
func (m Message) Text() string {
	return contentText(m.Content)
}

func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var b strings.Builder
		for _, p := range parts {
			if p.Type == "text" || p.Type == "" {
				if b.Len() > 0 {
					b.WriteString("\n")
				}
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
	return ""
}

// TextContent builds a string content value.
func TextContent(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

// ChatRequest is the OpenAI /v1/chat/completions payload. Unknown fields are
// retained in Extra so OpenAI-compatible upstreams receive them unchanged.
type ChatRequest struct {
	Model            string          `json:"model"`
	Messages         []Message       `json:"messages"`
	Stream           bool            `json:"stream,omitempty"`
	Temperature      *float64        `json:"temperature,omitempty"`
	TopP             *float64        `json:"top_p,omitempty"`
	MaxTokens        *int            `json:"max_tokens,omitempty"`
	MaxCompletionTok *int            `json:"max_completion_tokens,omitempty"`
	Stop             json.RawMessage `json:"stop,omitempty"`
	N                *int            `json:"n,omitempty"`
	Tools            json.RawMessage `json:"tools,omitempty"`
	ToolChoice       json.RawMessage `json:"tool_choice,omitempty"`
	ResponseFormat   json.RawMessage `json:"response_format,omitempty"`
	StreamOptions    json.RawMessage `json:"stream_options,omitempty"`
	User             string          `json:"user,omitempty"`
	// Extra holds unknown client fields; it is merged into the wire object by
	// MarshalJSON and must never be serialised as a field itself.
	Extra map[string]json.RawMessage `json:"-"`
}

var knownFields = map[string]bool{
	"model": true, "messages": true, "stream": true, "temperature": true, "top_p": true,
	"max_tokens": true, "max_completion_tokens": true, "stop": true, "n": true, "tools": true,
	"tool_choice": true, "response_format": true, "stream_options": true, "user": true,
}

// UnmarshalJSON captures unknown fields into Extra.
func (r *ChatRequest) UnmarshalJSON(b []byte) error {
	type alias ChatRequest
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(b, &all); err != nil {
		return err
	}
	a.Extra = map[string]json.RawMessage{}
	for k, v := range all {
		if !knownFields[k] {
			a.Extra[k] = v
		}
	}
	*r = ChatRequest(a)
	return nil
}

// MarshalJSON flattens Extra back into the object.
func (r ChatRequest) MarshalJSON() ([]byte, error) {
	type alias ChatRequest
	a := alias(r)
	a.Extra = nil
	base, err := json.Marshal(a)
	if err != nil {
		return nil, err
	}
	if len(r.Extra) == 0 {
		return base, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(base, &m); err != nil {
		return nil, err
	}
	for k, v := range r.Extra {
		m[k] = v
	}
	return json.Marshal(m)
}

// MaxOutputTokens resolves whichever max-tokens field the client set.
func (r ChatRequest) MaxOutputTokens() int {
	if r.MaxCompletionTok != nil {
		return *r.MaxCompletionTok
	}
	if r.MaxTokens != nil {
		return *r.MaxTokens
	}
	return 0
}

// StopSequences normalises the stop field into a slice.
func (r ChatRequest) StopSequences() []string {
	if len(r.Stop) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(r.Stop, &s); err == nil {
		return []string{s}
	}
	var arr []string
	if err := json.Unmarshal(r.Stop, &arr); err == nil {
		return arr
	}
	return nil
}

// Usage mirrors OpenAI's usage block. PromptTokens always includes the
// cached part of the prompt, whichever provider answered, so the number
// means the same thing on every connection and prompt + completion == total.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	// PromptTokensDetails and CompletionTokensDetails are OpenAI's
	// breakdowns; each adapter fills in what its own provider reports.
	PromptTokensDetails     *PromptTokensDetails     `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *CompletionTokensDetails `json:"completion_tokens_details,omitempty"`
	// PromptCacheHitTokens and PromptCacheMissTokens are DeepSeek's
	// top-level spelling of the same split; normalize folds them in.
	PromptCacheHitTokens  int `json:"prompt_cache_hit_tokens,omitempty"`
	PromptCacheMissTokens int `json:"prompt_cache_miss_tokens,omitempty"`
}

// PromptTokensDetails breaks the prompt down by how it was billed.
type PromptTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
	// CacheWriteTokens has no OpenAI equivalent: OpenAI does not bill cache
	// writes, Anthropic does.
	CacheWriteTokens int `json:"cache_creation_tokens,omitempty"`
	AudioTokens      int `json:"audio_tokens,omitempty"`
}

// CompletionTokensDetails carries the reasoning share of the completion.
type CompletionTokensDetails struct {
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
}

// normalize folds provider-specific spellings into the OpenAI shape and
// computes a total the upstream left out. It is safe on a nil receiver so
// callers can apply it to an optional usage block unconditionally.
func (u *Usage) normalize() {
	if u == nil {
		return
	}
	if u.PromptCacheHitTokens > 0 && u.CachedTokens() == 0 {
		u.promptDetails().CachedTokens = u.PromptCacheHitTokens
	}
	if u.TotalTokens == 0 {
		u.TotalTokens = u.PromptTokens + u.CompletionTokens
	}
}

// promptDetails returns the prompt breakdown, creating it on first use.
func (u *Usage) promptDetails() *PromptTokensDetails {
	if u.PromptTokensDetails == nil {
		u.PromptTokensDetails = &PromptTokensDetails{}
	}
	return u.PromptTokensDetails
}

// CachedTokens is the part of the prompt a provider cache served.
func (u *Usage) CachedTokens() int {
	if u == nil || u.PromptTokensDetails == nil {
		return 0
	}
	return u.PromptTokensDetails.CachedTokens
}

// CacheWriteTokens is the part of the prompt written into a provider cache.
func (u *Usage) CacheWriteTokens() int {
	if u == nil || u.PromptTokensDetails == nil {
		return 0
	}
	return u.PromptTokensDetails.CacheWriteTokens
}

// Choice is one non-streaming completion choice.
type Choice struct {
	Index        int             `json:"index"`
	Message      ResponseMessage `json:"message"`
	FinishReason *string         `json:"finish_reason"`
}

// ResponseMessage is the assistant reply.
type ResponseMessage struct {
	Role      string          `json:"role"`
	Content   *string         `json:"content"`
	ToolCalls json.RawMessage `json:"tool_calls,omitempty"`
}

// ChatResponse is the OpenAI non-streaming response.
type ChatResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   *Usage   `json:"usage,omitempty"`
}

// Delta is the incremental payload in a streaming choice.
type Delta struct {
	Role      string          `json:"role,omitempty"`
	Content   *string         `json:"content,omitempty"`
	ToolCalls json.RawMessage `json:"tool_calls,omitempty"`
}

// StreamChoice is one streaming choice.
type StreamChoice struct {
	Index        int     `json:"index"`
	Delta        Delta   `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

// StreamChunk is one OpenAI "chat.completion.chunk" event.
type StreamChunk struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []StreamChoice `json:"choices"`
	Usage   *Usage         `json:"usage,omitempty"`
}

// Provider is a chat backend.
type Provider interface {
	// Chat performs a blocking completion.
	Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)
	// ChatStream emits normalised chunks on out until the completion ends.
	// The implementation closes nothing; the caller owns the channel.
	ChatStream(ctx context.Context, req ChatRequest, out chan<- StreamChunk) error
}

// Embedder produces vector embeddings.
type Embedder interface {
	Embed(ctx context.Context, inputs []string) ([][]float32, error)
}

// Error is an upstream failure carrying an HTTP status to relay.
type Error struct {
	Status  int
	Type    string
	Code    string
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("upstream %d: %s", e.Status, e.Message)
}

// ErrorJSON renders the error in OpenAI's envelope.
func (e *Error) ErrorJSON() []byte {
	b, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": e.Message,
			"type":    e.Type,
			"code":    e.Code,
		},
	})
	return b
}

func strPtr(s string) *string { return &s }
