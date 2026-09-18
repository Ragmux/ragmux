package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

const geminiBase = "https://generativelanguage.googleapis.com/v1beta"

type gemini struct {
	cfg  Config
	base string
}

func newGemini(cfg Config) *gemini {
	return &gemini{cfg: cfg, base: cfg.baseURL(geminiBase)}
}

func (p *gemini) headers() map[string]string {
	return map[string]string{"x-goog-api-key": p.cfg.APIKey}
}

type geminiPart struct {
	Text       string          `json:"text,omitempty"`
	InlineData *geminiInline   `json:"inlineData,omitempty"`
	FileData   *geminiFileData `json:"fileData,omitempty"`
}

type geminiInline struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type geminiFileData struct {
	MimeType string `json:"mimeType,omitempty"`
	FileURI  string `json:"fileUri"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiRequest struct {
	SystemInstruction *geminiContent  `json:"systemInstruction,omitempty"`
	Contents          []geminiContent `json:"contents"`
	GenerationConfig  map[string]any  `json:"generationConfig,omitempty"`
}

type geminiResponse struct {
	Candidates []struct {
		Content      geminiContent `json:"content"`
		FinishReason string        `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
		ThoughtsTokenCount   int `json:"thoughtsTokenCount"`
		TotalTokenCount      int `json:"totalTokenCount"`
	} `json:"usageMetadata"`
}

func translateGemini(req ChatRequest) (geminiRequest, error) {
	out := geminiRequest{}
	if len(req.Tools) > 0 {
		return out, &Error{Status: http.StatusBadRequest, Type: "invalid_request_error",
			Message: "tool calling is not supported for gemini connections in this gateway version"}
	}
	var system []string
	for _, m := range req.Messages {
		switch m.Role {
		case "system", "developer":
			system = append(system, m.Text())
		case "user":
			parts, err := openAIPartsToGemini(m.Content)
			if err != nil {
				return out, err
			}
			out.Contents = appendGemini(out.Contents, "user", parts)
		case "assistant":
			if t := m.Text(); t != "" {
				out.Contents = appendGemini(out.Contents, "model", []geminiPart{{Text: t}})
			}
		case "tool":
			out.Contents = appendGemini(out.Contents, "user", []geminiPart{{Text: m.Text()}})
		}
	}
	if len(system) > 0 {
		out.SystemInstruction = &geminiContent{Parts: []geminiPart{{Text: strings.Join(system, "\n\n")}}}
	}
	gc := map[string]any{}
	if req.Temperature != nil {
		gc["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		gc["topP"] = *req.TopP
	}
	if n := req.MaxOutputTokens(); n > 0 {
		gc["maxOutputTokens"] = n
	}
	if stops := req.StopSequences(); len(stops) > 0 {
		gc["stopSequences"] = stops
	}
	if len(req.ResponseFormat) > 0 {
		var rf struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(req.ResponseFormat, &rf) == nil && strings.HasPrefix(rf.Type, "json") {
			gc["responseMimeType"] = "application/json"
		}
	}
	if len(gc) > 0 {
		out.GenerationConfig = gc
	}
	if len(out.Contents) == 0 {
		return out, &Error{Status: http.StatusBadRequest, Type: "invalid_request_error", Message: "messages must contain at least one user message"}
	}
	return out, nil
}

func appendGemini(cs []geminiContent, role string, parts []geminiPart) []geminiContent {
	if n := len(cs); n > 0 && cs[n-1].Role == role {
		cs[n-1].Parts = append(cs[n-1].Parts, parts...)
		return cs
	}
	return append(cs, geminiContent{Role: role, Parts: parts})
}

func openAIPartsToGemini(raw json.RawMessage) ([]geminiPart, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []geminiPart{{Text: s}}, nil
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, &Error{Status: http.StatusBadRequest, Type: "invalid_request_error", Message: "unsupported message content"}
	}
	var out []geminiPart
	for _, p := range parts {
		switch p.Type {
		case "text":
			out = append(out, geminiPart{Text: p.Text})
		case "image_url":
			u := p.ImageURL.URL
			if strings.HasPrefix(u, "data:") {
				meta, data, ok := strings.Cut(strings.TrimPrefix(u, "data:"), ",")
				if !ok {
					continue
				}
				out = append(out, geminiPart{InlineData: &geminiInline{MimeType: strings.TrimSuffix(meta, ";base64"), Data: data}})
			} else {
				out = append(out, geminiPart{FileData: &geminiFileData{FileURI: u}})
			}
		}
	}
	if len(out) == 0 {
		out = []geminiPart{{Text: ""}}
	}
	return out, nil
}

func geminiFinish(reason string) *string {
	switch reason {
	case "STOP":
		return strPtr("stop")
	case "MAX_TOKENS":
		return strPtr("length")
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII":
		return strPtr("content_filter")
	case "":
		return nil
	}
	return strPtr("stop")
}

func geminiText(c geminiContent) string {
	var b strings.Builder
	for _, p := range c.Parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

func (p *gemini) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	body, err := translateGemini(req)
	if err != nil {
		return nil, err
	}
	var gr geminiResponse
	url := p.base + "/models/" + p.cfg.Model + ":generateContent"
	if err := doJSON(ctx, p.cfg, url, p.headers(), body, &gr); err != nil {
		return nil, err
	}
	resp := &ChatResponse{ID: chatID(), Object: "chat.completion", Created: time.Now().Unix(), Model: req.Model,
		Usage: geminiUsage(gr)}
	if len(gr.Candidates) == 0 {
		resp.Choices = []Choice{{Index: 0, Message: ResponseMessage{Role: "assistant", Content: strPtr("")}, FinishReason: strPtr("content_filter")}}
		return resp, nil
	}
	for i, c := range gr.Candidates {
		resp.Choices = append(resp.Choices, Choice{Index: i,
			Message:      ResponseMessage{Role: "assistant", Content: strPtr(geminiText(c.Content))},
			FinishReason: geminiFinish(c.FinishReason)})
	}
	return resp, nil
}

func (p *gemini) ChatStream(ctx context.Context, req ChatRequest, out chan<- StreamChunk) error {
	body, err := translateGemini(req)
	if err != nil {
		return err
	}
	url := p.base + "/models/" + p.cfg.Model + ":streamGenerateContent?alt=sse"
	resp, err := doStream(ctx, p.cfg, url, p.headers(), body)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	id := chatID()
	created := time.Now().Unix()
	usage := &Usage{}
	sentRole := false
	emit := func(c StreamChunk) bool {
		c.ID, c.Object, c.Created, c.Model = id, "chat.completion.chunk", created, req.Model
		select {
		case out <- c:
			return true
		case <-ctx.Done():
			return false
		}
	}
	err = readSSE(resp.Body, func(ev sseEvent) bool {
		var gr geminiResponse
		if json.Unmarshal([]byte(ev.Data), &gr) != nil {
			return true
		}
		if gr.UsageMetadata.TotalTokenCount > 0 {
			usage = geminiUsage(gr)
		}
		for _, c := range gr.Candidates {
			text := geminiText(c.Content)
			delta := Delta{Content: strPtr(text)}
			if !sentRole {
				delta.Role = "assistant"
				sentRole = true
			}
			if !emit(StreamChunk{Choices: []StreamChoice{{Index: 0, Delta: delta}}}) {
				return false
			}
			if c.FinishReason != "" {
				if !emit(StreamChunk{Choices: []StreamChoice{{Index: 0, Delta: Delta{}, FinishReason: geminiFinish(c.FinishReason)}}}) {
					return false
				}
			}
		}
		return true
	})
	if err != nil {
		return err
	}
	emit(StreamChunk{Choices: []StreamChoice{}, Usage: usage})
	return nil
}

// geminiUsage maps usageMetadata to the OpenAI shape. Reasoning ("thoughts")
// tokens are billed as output, so they count as completion tokens, which keeps
// prompt + completion == total like the other providers.
func geminiUsage(gr geminiResponse) *Usage {
	u := gr.UsageMetadata
	return &Usage{PromptTokens: u.PromptTokenCount, CompletionTokens: u.CandidatesTokenCount + u.ThoughtsTokenCount,
		TotalTokens: u.TotalTokenCount}
}
