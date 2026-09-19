package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

// ollamaNativeBase is the default host of a local Ollama daemon.
const ollamaNativeBase = "http://localhost:11434"

// ollama speaks Ollama's native /api/chat protocol: JSON in, either one JSON
// object or newline-delimited JSON objects out. Using the native API instead
// of Ollama's OpenAI shim gives access to keep_alive, num_ctx and the rest of
// the model options; the shim remains reachable through custom_openai.
type ollama struct {
	cfg  Config
	base string
}

func newOllama(cfg Config) *ollama {
	base := strings.TrimSuffix(cfg.baseURL(ollamaNativeBase), "/v1")
	return &ollama{cfg: cfg, base: base}
}

func (p *ollama) headers() map[string]string {
	h := map[string]string{}
	if p.cfg.APIKey != "" {
		// Ollama itself is unauthenticated; the key is for a proxy in front.
		h["Authorization"] = "Bearer " + p.cfg.APIKey
	}
	return h
}

type ollamaMessage struct {
	Role      string           `json:"role"`
	Content   string           `json:"content"`
	Images    []string         `json:"images,omitempty"`
	ToolCalls []ollamaToolCall `json:"tool_calls,omitempty"`
	ToolName  string           `json:"tool_name,omitempty"`
}

type ollamaToolCall struct {
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

type ollamaRequest struct {
	Model     string          `json:"model"`
	Messages  []ollamaMessage `json:"messages"`
	Stream    bool            `json:"stream"`
	Options   map[string]any  `json:"options,omitempty"`
	KeepAlive json.RawMessage `json:"keep_alive,omitempty"`
	Tools     json.RawMessage `json:"tools,omitempty"`
	Format    json.RawMessage `json:"format,omitempty"`
}

// ollamaResponse is one /api/chat object; the streaming form emits many.
type ollamaResponse struct {
	Model           string        `json:"model"`
	Message         ollamaMessage `json:"message"`
	Done            bool          `json:"done"`
	DoneReason      string        `json:"done_reason"`
	PromptEvalCount int           `json:"prompt_eval_count"`
	EvalCount       int           `json:"eval_count"`
	Error           string        `json:"error"`
}

// translateOllama converts an OpenAI request into the native payload.
func translateOllama(req ChatRequest, model string) (ollamaRequest, error) {
	out := ollamaRequest{Model: model, Options: map[string]any{}}
	for _, m := range req.Messages {
		om := ollamaMessage{Role: m.Role}
		switch m.Role {
		case "developer":
			om.Role = "system"
			om.Content = m.Text()
		case "user":
			text, images, err := openAIPartsToOllama(m.Content)
			if err != nil {
				return out, err
			}
			om.Content, om.Images = text, images
		case "assistant":
			om.Content = m.Text()
			if len(m.ToolCalls) > 0 {
				var calls []struct {
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				}
				if err := json.Unmarshal(m.ToolCalls, &calls); err == nil {
					for _, c := range calls {
						var tc ollamaToolCall
						tc.Function.Name = c.Function.Name
						args := json.RawMessage(c.Function.Arguments)
						if !json.Valid(args) {
							args = json.RawMessage("{}")
						}
						tc.Function.Arguments = args
						om.ToolCalls = append(om.ToolCalls, tc)
					}
				}
			}
		case "tool":
			om.Content = m.Text()
			om.ToolName = m.Name
		default:
			om.Content = m.Text()
		}
		out.Messages = append(out.Messages, om)
	}
	if len(out.Messages) == 0 {
		return out, &Error{Status: http.StatusBadRequest, Type: "invalid_request_error", Message: "messages must contain at least one message"}
	}
	if req.Temperature != nil {
		out.Options["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out.Options["top_p"] = *req.TopP
	}
	if n := req.MaxOutputTokens(); n > 0 {
		out.Options["num_predict"] = n
	}
	if stop := req.StopSequences(); len(stop) > 0 {
		out.Options["stop"] = stop
	}
	if len(req.Tools) > 0 {
		// Ollama uses OpenAI's {type:"function", function:{...}} shape.
		out.Tools = req.Tools
	}
	if len(req.ResponseFormat) > 0 {
		var rf struct {
			Type       string `json:"type"`
			JSONSchema struct {
				Schema json.RawMessage `json:"schema"`
			} `json:"json_schema"`
		}
		if json.Unmarshal(req.ResponseFormat, &rf) == nil {
			switch {
			case rf.Type == "json_schema" && len(rf.JSONSchema.Schema) > 0:
				out.Format = rf.JSONSchema.Schema
			case rf.Type == "json_object":
				out.Format = json.RawMessage(`"json"`)
			}
		}
	}
	// Ollama-specific extras the OpenAI schema has no field for.
	if v, ok := req.Extra["keep_alive"]; ok {
		out.KeepAlive = v
	}
	if v, ok := req.Extra["num_ctx"]; ok {
		var n int
		if json.Unmarshal(v, &n) == nil {
			out.Options["num_ctx"] = n
		}
	}
	if v, ok := req.Extra["options"]; ok {
		var extra map[string]any
		if json.Unmarshal(v, &extra) == nil {
			for k, val := range extra {
				out.Options[k] = val
			}
		}
	}
	if len(out.Options) == 0 {
		out.Options = nil
	}
	return out, nil
}

// openAIPartsToOllama splits a user message into text and base64 images.
func openAIPartsToOllama(raw json.RawMessage) (string, []string, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil, nil
	}
	if len(raw) == 0 {
		return "", nil, nil
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", nil, &Error{Status: http.StatusBadRequest, Type: "invalid_request_error", Message: "unsupported message content"}
	}
	var text strings.Builder
	var images []string
	for _, p := range parts {
		switch p.Type {
		case "text", "":
			if text.Len() > 0 {
				text.WriteString("\n")
			}
			text.WriteString(p.Text)
		case "image_url":
			u := p.ImageURL.URL
			if !strings.HasPrefix(u, "data:") {
				return "", nil, &Error{Status: http.StatusBadRequest, Type: "invalid_request_error",
					Message: "ollama only accepts inline base64 images (data: URLs)"}
			}
			if _, data, ok := strings.Cut(u, ","); ok {
				images = append(images, data)
			}
		}
	}
	return text.String(), images, nil
}

// ollamaToolCallsJSON renders native tool calls in OpenAI's shape. Ollama
// sends no call ids, so the normaliser generates them.
func ollamaToolCallsJSON(calls []ollamaToolCall) json.RawMessage {
	out := make([]toolCall, len(calls))
	for i, c := range calls {
		out[i] = toolCall{Name: c.Function.Name, Arguments: string(c.Function.Arguments)}
	}
	return toolCallsJSON(out)
}

func ollamaFinish(r ollamaResponse) *string {
	if len(r.Message.ToolCalls) > 0 {
		return strPtr("tool_calls")
	}
	switch r.DoneReason {
	case "stop", "":
		return strPtr("stop")
	case "length":
		return strPtr("length")
	}
	return strPtr(r.DoneReason)
}

func ollamaUsage(r ollamaResponse) *Usage {
	return &Usage{PromptTokens: r.PromptEvalCount, CompletionTokens: r.EvalCount, TotalTokens: r.PromptEvalCount + r.EvalCount}
}

func (p *ollama) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	body, err := translateOllama(req, p.cfg.Model)
	if err != nil {
		return nil, err
	}
	var or ollamaResponse
	if err := doJSON(ctx, p.cfg, p.base+"/api/chat", p.headers(), body, &or); err != nil {
		return nil, err
	}
	if or.Error != "" {
		return nil, &Error{Status: http.StatusBadGateway, Type: "upstream_error", Message: RedactWith(or.Error, p.cfg.APIKey)}
	}
	msg := ResponseMessage{Role: "assistant", Content: strPtr(or.Message.Content)}
	msg.ToolCalls = ollamaToolCallsJSON(or.Message.ToolCalls)
	return &ChatResponse{
		ID: chatID(), Object: "chat.completion", Created: time.Now().Unix(), Model: req.Model,
		Choices: []Choice{{Index: 0, Message: msg, FinishReason: ollamaFinish(or)}},
		Usage:   ollamaUsage(or),
	}, nil
}

func (p *ollama) ChatStream(ctx context.Context, req ChatRequest, out chan<- StreamChunk) error {
	body, err := translateOllama(req, p.cfg.Model)
	if err != nil {
		return err
	}
	body.Stream = true
	resp, err := doStream(ctx, p.cfg, p.base+"/api/chat", p.headers(), body)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	id := chatID()
	created := time.Now().Unix()
	emit := func(c StreamChunk) bool {
		c.ID, c.Object, c.Created, c.Model = id, "chat.completion.chunk", created, req.Model
		select {
		case out <- c:
			return true
		case <-ctx.Done():
			return false
		}
	}
	sentRole := false
	// Scoped to the whole stream, not to one NDJSON line: newer builds spread
	// tool calls over several lines, and a per-line index would number every
	// one of them 0 so clients would merge them into a single corrupt call.
	var tools toolCallStream
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var or ollamaResponse
		if err := json.Unmarshal([]byte(line), &or); err != nil {
			continue
		}
		if or.Error != "" {
			return &Error{Status: http.StatusBadGateway, Type: "upstream_error", Message: RedactWith(or.Error, p.cfg.APIKey)}
		}
		delta := Delta{}
		if !sentRole {
			delta.Role = "assistant"
			sentRole = true
		}
		if or.Message.Content != "" || !or.Done {
			delta.Content = strPtr(or.Message.Content)
		}
		// Ollama delivers arguments atomically, so each call is one whole
		// delta. On the final line the last call rides along with the finish
		// reason, keeping the shape Ollama itself sends.
		calls := or.Message.ToolCalls
		last := len(calls)
		if or.Done && last > 0 {
			last--
		}
		sent := false
		for _, c := range calls[:last] {
			delta.ToolCalls = tools.Whole("", c.Function.Name, string(c.Function.Arguments))
			if !emit(StreamChunk{Choices: []StreamChoice{{Index: 0, Delta: delta}}}) {
				return ctx.Err()
			}
			delta, sent = Delta{}, true
		}
		if !or.Done {
			if !sent && !emit(StreamChunk{Choices: []StreamChoice{{Index: 0, Delta: delta}}}) {
				return ctx.Err()
			}
			continue
		}
		if len(calls) > 0 {
			delta.ToolCalls = tools.Whole("", calls[last].Function.Name, string(calls[last].Function.Arguments))
		}
		finish := ollamaFinish(or)
		if tools.Len() > 0 {
			finish = strPtr("tool_calls")
		}
		if !emit(StreamChunk{Choices: []StreamChoice{{Index: 0, Delta: delta, FinishReason: finish}}}) {
			return ctx.Err()
		}
		// Usage-only trailer, mirroring OpenAI's include_usage behaviour.
		if !emit(StreamChunk{Choices: []StreamChoice{}, Usage: ollamaUsage(or)}) {
			return ctx.Err()
		}
		return nil
	}
	if err := sc.Err(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var pe *Error
		if errors.As(err, &pe) {
			return pe
		}
		return &Error{Status: http.StatusBadGateway, Type: "upstream_error", Message: "read upstream stream: " + RedactWith(err.Error(), p.cfg.APIKey)}
	}
	return io.ErrUnexpectedEOF
}
