package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

const (
	anthropicBase    = "https://api.anthropic.com"
	anthropicVersion = "2023-06-01"
	defaultMaxTokens = 4096
)

type anthropic struct {
	cfg  Config
	base string
}

func newAnthropic(cfg Config) *anthropic {
	base := cfg.baseURL(anthropicBase)
	base = strings.TrimSuffix(base, "/v1")
	return &anthropic{cfg: cfg, base: base}
}

func (p *anthropic) headers() map[string]string {
	return map[string]string{
		"x-api-key":         p.cfg.APIKey,
		"anthropic-version": anthropicVersion,
	}
}

// anthropicRequest is the Messages API payload. System is raw because the
// field is either a plain string or an array of blocks; see anthropicSystem.
type anthropicRequest struct {
	Model         string             `json:"model"`
	System        json.RawMessage    `json:"system,omitempty"`
	Messages      []anthropicMessage `json:"messages"`
	MaxTokens     int                `json:"max_tokens"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
	Tools         []anthropicTool    `json:"tools,omitempty"`
	ToolChoice    json.RawMessage    `json:"tool_choice,omitempty"`
}

type anthropicMessage struct {
	Role    string             `json:"role"`
	Content []anthropicContent `json:"content"`
}

type anthropicContent struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
	Source    *anthropicImage `json:"source,omitempty"`
	// CacheControl is the client's prompt-caching breakpoint, relayed
	// verbatim: the gateway does not interpret its contents.
	CacheControl json.RawMessage `json:"cache_control,omitempty"`
}

type anthropicImage struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type anthropicTool struct {
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"input_schema"`
	CacheControl json.RawMessage `json:"cache_control,omitempty"`
}

// anthropicUsageBlock is the Messages API usage object. input_tokens counts
// only what was neither read from nor written to the cache.
type anthropicUsageBlock struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// usage maps Anthropic's counters onto the OpenAI shape. Anthropic excludes
// both cache counters from input_tokens while OpenAI's prompt_tokens
// includes cached_tokens, so they are added in: prompt_tokens then compares
// across providers and prompt + completion stays equal to total.
func (b anthropicUsageBlock) usage() *Usage {
	prompt := b.InputTokens + b.CacheCreationInputTokens + b.CacheReadInputTokens
	u := &Usage{PromptTokens: prompt, CompletionTokens: b.OutputTokens, TotalTokens: prompt + b.OutputTokens}
	if b.CacheReadInputTokens > 0 || b.CacheCreationInputTokens > 0 {
		u.PromptTokensDetails = &PromptTokensDetails{
			CachedTokens:     b.CacheReadInputTokens,
			CacheWriteTokens: b.CacheCreationInputTokens,
		}
	}
	return u
}

// merge copies the non-zero counters of other over b. Newer API versions
// repeat the cache fields in message_delta; an omitted (zero) field there
// must not erase what message_start already reported.
func (b *anthropicUsageBlock) merge(other anthropicUsageBlock) {
	for _, f := range []struct{ dst, src *int }{
		{&b.InputTokens, &other.InputTokens},
		{&b.OutputTokens, &other.OutputTokens},
		{&b.CacheCreationInputTokens, &other.CacheCreationInputTokens},
		{&b.CacheReadInputTokens, &other.CacheReadInputTokens},
	} {
		if *f.src != 0 {
			*f.dst = *f.src
		}
	}
}

type anthropicResponse struct {
	ID         string              `json:"id"`
	Type       string              `json:"type"`
	Role       string              `json:"role"`
	Model      string              `json:"model"`
	Content    []anthropicContent  `json:"content"`
	StopReason string              `json:"stop_reason"`
	Usage      anthropicUsageBlock `json:"usage"`
}

// translateAnthropic converts an OpenAI request into the Messages format.
func translateAnthropic(ctx context.Context, cfg Config, req ChatRequest) (anthropicRequest, error) {
	out := anthropicRequest{Model: cfg.Model, MaxTokens: defaultMaxTokens}
	images := cfg.imageBudget(ctx)
	if n := req.MaxOutputTokens(); n > 0 {
		out.MaxTokens = n
	}
	out.Temperature = req.Temperature
	out.TopP = req.TopP
	out.StopSequences = req.StopSequences()

	var system []anthropicContent
	for _, m := range req.Messages {
		switch m.Role {
		case "system", "developer":
			system = append(system, anthropicContent{Type: "text", Text: m.Text(), CacheControl: contentCacheControl(m.Content)})
		case "user":
			parts, err := openAIPartsToAnthropic(images, m.Content)
			if err != nil {
				return out, err
			}
			out.Messages = appendAnthropic(out.Messages, "user", parts)
		case "assistant":
			var parts []anthropicContent
			if t := m.Text(); t != "" {
				parts = append(parts, anthropicContent{Type: "text", Text: t})
			}
			if len(m.ToolCalls) > 0 {
				var calls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				}
				if err := json.Unmarshal(m.ToolCalls, &calls); err == nil {
					for _, c := range calls {
						args := json.RawMessage(c.Function.Arguments)
						if !json.Valid(args) {
							args = json.RawMessage("{}")
						}
						parts = append(parts, anthropicContent{Type: "tool_use", ID: c.ID, Name: c.Function.Name, Input: args})
					}
				}
			}
			if len(parts) == 0 {
				continue
			}
			out.Messages = appendAnthropic(out.Messages, "assistant", parts)
		case "tool":
			out.Messages = appendAnthropic(out.Messages, "user", []anthropicContent{{
				Type: "tool_result", ToolUseID: m.ToolCallID, Content: m.Text(),
			}})
		}
	}
	if len(system) > 0 {
		sys, err := anthropicSystem(system)
		if err != nil {
			return out, err
		}
		out.System = sys
	}
	if len(req.Tools) > 0 {
		var tools []struct {
			Type         string          `json:"type"`
			CacheControl json.RawMessage `json:"cache_control"`
			Function     struct {
				Name         string          `json:"name"`
				Description  string          `json:"description"`
				Parameters   json.RawMessage `json:"parameters"`
				CacheControl json.RawMessage `json:"cache_control"`
			} `json:"function"`
		}
		if err := json.Unmarshal(req.Tools, &tools); err == nil {
			for _, t := range tools {
				schema := t.Function.Parameters
				if len(schema) == 0 {
					schema = json.RawMessage(`{"type":"object","properties":{}}`)
				}
				// Clients spell the breakpoint either on the tool object or
				// inside function, depending on which SDK wrote the request.
				cc := t.CacheControl
				if len(cc) == 0 {
					cc = t.Function.CacheControl
				}
				out.Tools = append(out.Tools, anthropicTool{Name: t.Function.Name, Description: t.Function.Description,
					InputSchema: schema, CacheControl: cc})
			}
		}
		if len(req.ToolChoice) > 0 {
			var s string
			if json.Unmarshal(req.ToolChoice, &s) == nil {
				switch s {
				case "auto":
					out.ToolChoice = json.RawMessage(`{"type":"auto"}`)
				case "required":
					out.ToolChoice = json.RawMessage(`{"type":"any"}`)
				case "none":
					out.Tools = nil
				}
			} else {
				var obj struct {
					Function struct {
						Name string `json:"name"`
					} `json:"function"`
				}
				if json.Unmarshal(req.ToolChoice, &obj) == nil && obj.Function.Name != "" {
					b, _ := json.Marshal(map[string]string{"type": "tool", "name": obj.Function.Name})
					out.ToolChoice = b
				}
			}
		}
	}
	if len(out.Messages) == 0 {
		return out, &Error{Status: http.StatusBadRequest, Type: "invalid_request_error", Message: "messages must contain at least one user message"}
	}
	return out, nil
}

// appendAnthropic merges consecutive same-role messages, which the Messages
// API requires.
func appendAnthropic(msgs []anthropicMessage, role string, parts []anthropicContent) []anthropicMessage {
	if n := len(msgs); n > 0 && msgs[n-1].Role == role {
		msgs[n-1].Content = append(msgs[n-1].Content, parts...)
		return msgs
	}
	return append(msgs, anthropicMessage{Role: role, Content: parts})
}

// anthropicSystem renders the system blocks. Without a single cache_control
// the value is the joined plain string Anthropic has always been sent, so a
// request that does not ask for caching keeps its previous wire shape byte
// for byte; one breakpoint switches to the block array that can carry it.
func anthropicSystem(blocks []anthropicContent) (json.RawMessage, error) {
	texts := make([]string, len(blocks))
	cached := false
	for i, b := range blocks {
		texts[i] = b.Text
		if len(b.CacheControl) > 0 {
			cached = true
		}
	}
	if !cached {
		return json.Marshal(strings.Join(texts, "\n\n"))
	}
	return json.Marshal(blocks)
}

// contentCacheControl returns the cache_control a client attached to a
// parts-array content, or nil. The last marker wins: Anthropic caches the
// prefix up to and including the marked block, so with several markers in
// one message the final one is the breakpoint that covers all of it.
func contentCacheControl(raw json.RawMessage) json.RawMessage {
	parts, err := parseContent(raw, imageMediaTypesFor("anthropic"))
	if err != nil {
		return nil
	}
	var cc json.RawMessage
	for _, p := range parts {
		if len(p.CacheControl) > 0 {
			cc = p.CacheControl
		}
	}
	return cc
}

func openAIPartsToAnthropic(images *imageBudget, raw json.RawMessage) ([]anthropicContent, error) {
	parts, err := parseContent(raw, images.accepted())
	if err != nil {
		return nil, err
	}
	var out []anthropicContent
	for _, p := range parts {
		if p.Type == "text" {
			out = append(out, anthropicContent{Type: "text", Text: p.Text, CacheControl: p.CacheControl})
			continue
		}
		ref := p.Image
		if anthropicInlineImages {
			if ref, err = images.inline(ref); err != nil {
				return nil, err
			}
		}
		switch {
		case ref.Base64 != "":
			out = append(out, anthropicContent{Type: "image", CacheControl: p.CacheControl,
				Source: &anthropicImage{Type: "base64", MediaType: ref.MediaType, Data: ref.Base64}})
		case ref.URL != "":
			out = append(out, anthropicContent{Type: "image", CacheControl: p.CacheControl,
				Source: &anthropicImage{Type: "url", URL: ref.URL}})
		}
	}
	if len(out) == 0 {
		out = []anthropicContent{{Type: "text", Text: ""}}
	}
	return out, nil
}

func anthropicFinish(stop string) *string {
	switch stop {
	case "end_turn", "stop_sequence":
		return strPtr("stop")
	case "max_tokens":
		return strPtr("length")
	case "tool_use":
		return strPtr("tool_calls")
	case "":
		return nil
	}
	return strPtr(stop)
}

func (p *anthropic) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	body, err := translateAnthropic(ctx, p.cfg, req)
	if err != nil {
		return nil, err
	}
	var ar anthropicResponse
	if err := doJSON(ctx, p.cfg, opChat, p.base+"/v1/messages", p.headers(), body, &ar); err != nil {
		return nil, err
	}
	var text strings.Builder
	var calls []toolCall
	for _, c := range ar.Content {
		switch c.Type {
		case "text":
			text.WriteString(c.Text)
		case "tool_use":
			calls = append(calls, toolCall{ID: c.ID, Name: c.Name, Arguments: string(c.Input)})
		}
	}
	msg := ResponseMessage{Role: "assistant", Content: strPtr(text.String()), ToolCalls: toolCallsJSON(calls)}
	id := ar.ID
	if id == "" {
		id = chatID()
	}
	return &ChatResponse{
		ID: id, Object: "chat.completion", Created: time.Now().Unix(), Model: req.Model,
		Choices: []Choice{{Index: 0, Message: msg, FinishReason: anthropicFinish(ar.StopReason)}},
		Usage:   ar.Usage.usage(),
	}, nil
}

func (p *anthropic) ChatStream(ctx context.Context, req ChatRequest, out chan<- StreamChunk) error {
	body, err := translateAnthropic(ctx, p.cfg, req)
	if err != nil {
		return err
	}
	body.Stream = true
	resp, err := doStream(ctx, p.cfg, p.base+"/v1/messages", p.headers(), body)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	id := chatID()
	created := time.Now().Unix()
	var usage anthropicUsageBlock
	emit := func(c StreamChunk) bool {
		c.ID, c.Object, c.Created, c.Model = id, "chat.completion.chunk", created, req.Model
		select {
		case out <- c:
			return true
		case <-ctx.Done():
			return false
		}
	}
	// Tool_use blocks are tracked by content index so argument deltas map to
	// the right OpenAI tool_calls index.
	var tools toolCallStream
	var streamErr error
	sentRole := false

	err = readSSE(resp.Body, func(ev sseEvent) bool {
		var base struct {
			Type string `json:"type"`
		}
		if json.Unmarshal([]byte(ev.Data), &base) != nil {
			return true
		}
		switch base.Type {
		case "message_start":
			var ms struct {
				Message struct {
					ID    string              `json:"id"`
					Usage anthropicUsageBlock `json:"usage"`
				} `json:"message"`
			}
			if json.Unmarshal([]byte(ev.Data), &ms) == nil {
				if ms.Message.ID != "" {
					id = ms.Message.ID
				}
				usage.merge(ms.Message.Usage)
			}
			sentRole = true
			return emit(StreamChunk{Choices: []StreamChoice{{Index: 0, Delta: Delta{Role: "assistant", Content: strPtr("")}}}})
		case "content_block_start":
			var cb struct {
				Index        int `json:"index"`
				ContentBlock struct {
					Type string `json:"type"`
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"content_block"`
			}
			if json.Unmarshal([]byte(ev.Data), &cb) == nil && cb.ContentBlock.Type == "tool_use" {
				tc := tools.Open(cb.Index, cb.ContentBlock.ID, cb.ContentBlock.Name)
				return emit(StreamChunk{Choices: []StreamChoice{{Index: 0, Delta: Delta{ToolCalls: tc}}}})
			}
			return true
		case "content_block_delta":
			var d struct {
				Index int `json:"index"`
				Delta struct {
					Type        string `json:"type"`
					Text        string `json:"text"`
					PartialJSON string `json:"partial_json"`
				} `json:"delta"`
			}
			if json.Unmarshal([]byte(ev.Data), &d) != nil {
				return true
			}
			switch d.Delta.Type {
			case "text_delta":
				delta := Delta{Content: strPtr(d.Delta.Text)}
				if !sentRole {
					delta.Role = "assistant"
					sentRole = true
				}
				return emit(StreamChunk{Choices: []StreamChoice{{Index: 0, Delta: delta}}})
			case "input_json_delta":
				tc := tools.Args(d.Index, d.Delta.PartialJSON)
				if tc == nil {
					return true
				}
				return emit(StreamChunk{Choices: []StreamChoice{{Index: 0, Delta: Delta{ToolCalls: tc}}}})
			}
			return true
		case "message_delta":
			var md struct {
				Delta struct {
					StopReason string `json:"stop_reason"`
				} `json:"delta"`
				Usage anthropicUsageBlock `json:"usage"`
			}
			if json.Unmarshal([]byte(ev.Data), &md) != nil {
				return true
			}
			usage.merge(md.Usage)
			return emit(StreamChunk{Choices: []StreamChoice{{Index: 0, Delta: Delta{}, FinishReason: anthropicFinish(md.Delta.StopReason)}}})
		case "message_stop":
			// Final usage-only chunk, mirroring OpenAI's include_usage behaviour.
			return emit(StreamChunk{Choices: []StreamChoice{}, Usage: usage.usage()})
		case "error":
			var e struct {
				Error struct {
					Type    string `json:"type"`
					Message string `json:"message"`
				} `json:"error"`
			}
			_ = json.Unmarshal([]byte(ev.Data), &e) // best effort; an empty message is still an error
			streamErr = &Error{Status: http.StatusBadGateway, Type: e.Error.Type, Message: RedactWith(e.Error.Message, p.cfg.APIKey)}
			return false
		}
		return true
	})
	if streamErr != nil {
		return streamErr
	}
	return err
}
