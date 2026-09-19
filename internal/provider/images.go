package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	neturl "net/url"
	"strings"
	"time"
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

// Image inlining policy, one constant per adapter so moving a provider from
// one side to the other is a single line:
//   - Ollama accepts nothing but inline base64.
//   - Gemini's fileData takes Files API and gs:// URIs only, so every other
//     URL has to arrive as bytes.
//   - Anthropic fetches a url image source itself, which is cheaper and puts
//     fewer bytes on our wire. A Bedrock-style gateway in front of Anthropic
//     that has no url source only has to flip this.
const (
	anthropicInlineImages = false
	geminiInlineImages    = true
	ollamaInlineImages    = true
)

// imageMediaTypes is what the gateway will inline. The URL's extension is
// never consulted; the response's own content type decides, because a URL can
// be spelled to claim anything.
var imageMediaTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}

// ImageFetcher resolves remote image URLs into base64 for adapters whose
// upstream cannot fetch a URL itself.
//
// It must be given the same netguard-hardened client the adapters use. An
// image URL comes from the API client, so it is exactly the kind of input the
// dialer's private-address filter and the redirect policy exist for; a second
// outbound path would be a second thing to keep safe.
type ImageFetcher struct {
	// Client is required.
	Client *http.Client
	// MaxBytes caps one image (default 8 MiB).
	MaxBytes int64
	// Timeout bounds one image fetch (default 10s).
	Timeout time.Duration
	// MaxPerRequest caps the images one chat request may pull (default 8).
	MaxPerRequest int
	// Cache is optional; nil fetches every time.
	Cache  *imageCache
	Logger *slog.Logger
}

// Fetch downloads one image and returns its media type and base64 payload.
// Everything a client can get wrong is a 400 naming only the host: the full
// URL may carry a signed query string, and it is the client's own string
// anyway. Transport and policy failures go through transportError so a
// private-address image URL reads like a private base URL.
func (f *ImageFetcher) Fetch(ctx context.Context, rawURL string) (string, string, error) {
	u, err := neturl.Parse(rawURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", "", imageError("image URLs must be http or https")
	}
	if mt, data, ok := f.Cache.get(rawURL); ok {
		return mt, data, nil
	}
	// Refuse rather than fall back to http.DefaultClient. This is the one
	// place the gateway follows a URL a client chose, and it is only safe
	// because it goes out over the same guarded transport as a provider call:
	// an unguarded default client here would reach loopback and private
	// addresses, which is exactly what netguard exists to prevent.
	if f.Client == nil {
		return "", "", imageError("remote image fetching is not configured")
	}
	// The fetcher has no connection of its own; ProviderType and Logger are
	// what transportError needs to log and classify the failure.
	cfg := Config{ProviderType: "image", Logger: f.Logger}
	ctx, cancel := context.WithTimeout(ctx, f.timeout())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", "", imageError("image URL is not a valid request target")
	}
	req.Header.Set("Accept", "image/*")
	req.Header.Set("User-Agent", "ragmux/1.0")
	resp, err := f.Client.Do(req)
	if err != nil {
		return "", "", transportError(cfg, rawURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 400 rather than 502: the client picked the host, not the operator.
		return "", "", imageError(fmt.Sprintf("image URL returned %d", resp.StatusCode))
	}
	mediaType, _, _ := strings.Cut(resp.Header.Get("Content-Type"), ";")
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	if !imageMediaTypes[mediaType] {
		return "", "", imageError(fmt.Sprintf("image URL at %s returned content type %q; png, jpeg, gif and webp are accepted",
			u.Host, mediaType))
	}
	if resp.ContentLength > f.maxBytes() {
		return "", "", imageError(f.tooLarge(u.Host))
	}
	// Read one byte past the limit: an absent or lying Content-Length must not
	// be able to spend more memory than the cap allows.
	body, err := io.ReadAll(io.LimitReader(resp.Body, f.maxBytes()+1))
	if err != nil {
		return "", "", transportError(cfg, rawURL, err)
	}
	if int64(len(body)) > f.maxBytes() {
		return "", "", imageError(f.tooLarge(u.Host))
	}
	data := base64.StdEncoding.EncodeToString(body)
	if !strings.Contains(strings.ToLower(resp.Header.Get("Cache-Control")), "no-store") {
		f.Cache.put(rawURL, mediaType, data)
	}
	return mediaType, data, nil
}

func (f *ImageFetcher) tooLarge(host string) string {
	return fmt.Sprintf("image at %s is larger than the %d MiB limit", host, f.maxBytes()>>20)
}

func (f *ImageFetcher) maxBytes() int64 {
	if f.MaxBytes > 0 {
		return f.MaxBytes
	}
	return 8 << 20
}

func (f *ImageFetcher) timeout() time.Duration {
	if f.Timeout > 0 {
		return f.Timeout
	}
	return 10 * time.Second
}

func (f *ImageFetcher) maxPerRequest() int {
	if f.MaxPerRequest > 0 {
		return f.MaxPerRequest
	}
	return 8
}

// imageBudget is one chat request's share of the fetcher: images are pulled
// one at a time and only so many of them, so a single client request cannot
// fan out into a burst of outbound connections.
type imageBudget struct {
	ctx   context.Context
	cfg   Config
	taken int
}

func (c Config) imageBudget(ctx context.Context) *imageBudget {
	return &imageBudget{ctx: ctx, cfg: c}
}

// inline resolves a remote image into base64. A reference that is already
// inline, or a gateway with image fetching switched off, is returned
// unchanged — the adapter then applies whatever it did before.
func (b *imageBudget) inline(ref imageRef) (imageRef, error) {
	if b == nil || b.cfg.Images == nil || ref.URL == "" {
		return ref, nil
	}
	if b.taken >= b.cfg.Images.maxPerRequest() {
		return ref, imageError(fmt.Sprintf("a chat request may fetch at most %d remote images", b.cfg.Images.maxPerRequest()))
	}
	b.taken++
	mediaType, data, err := b.cfg.Images.Fetch(b.ctx, ref.URL)
	if err != nil {
		return ref, err
	}
	return imageRef{MediaType: mediaType, Base64: data}, nil
}

func imageError(msg string) *Error {
	return &Error{Status: http.StatusBadRequest, Type: "invalid_request_error", Message: msg}
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
