package provider

import (
	"encoding/json"
	"net/http"
	"strings"
)

// contentPart is one OpenAI content part. Every adapter used to re-parse the
// raw content itself, which is how the three of them ended up disagreeing
// about parts without a type; they now all read this.
type contentPart struct {
	// Type is "text" or "image_url"; a part without a type is text, the way
	// OpenAI reads it.
	Type  string
	Text  string
	Image imageRef
	// CacheControl is the raw cache_control the client attached to the part.
	// Only Anthropic has anything to do with it; it is carried here so the
	// adapter that can use it does not have to parse the content again.
	CacheControl json.RawMessage
}

// imageRef is an image part with exactly one of Base64 (a data: URL, already
// split into media type and payload) or URL set.
type imageRef struct {
	MediaType string
	Base64    string
	URL       string
}

// parseContent normalises a message's content — a bare string or an array of
// parts — into content parts. Parts of a type no adapter can carry
// (input_audio, file, ...) are dropped rather than rejected: the model just
// does not see them, which beats failing a conversation over one part.
func parseContent(raw json.RawMessage) ([]contentPart, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []contentPart{{Type: "text", Text: s}}, nil
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL struct {
			URL string `json:"url"`
		} `json:"image_url"`
		CacheControl json.RawMessage `json:"cache_control"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, &Error{Status: http.StatusBadRequest, Type: "invalid_request_error", Message: "unsupported message content"}
	}
	out := make([]contentPart, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case "text", "":
			out = append(out, contentPart{Type: "text", Text: p.Text, CacheControl: p.CacheControl})
		case "image_url":
			ref, ok := parseImageURL(p.ImageURL.URL)
			if !ok {
				continue
			}
			out = append(out, contentPart{Type: "image_url", Image: ref, CacheControl: p.CacheControl})
		}
	}
	return out, nil
}

// parseImageURL splits a data: URL into its media type and payload, and keeps
// anything else as a URL for the adapter's own policy to resolve.
func parseImageURL(u string) (imageRef, bool) {
	if !strings.HasPrefix(u, "data:") {
		if u == "" {
			return imageRef{}, false
		}
		return imageRef{URL: u}, true
	}
	meta, data, ok := strings.Cut(strings.TrimPrefix(u, "data:"), ",")
	if !ok {
		return imageRef{}, false
	}
	return imageRef{MediaType: strings.TrimSuffix(meta, ";base64"), Base64: data}, true
}
