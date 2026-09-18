package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Config is the per-connection information adapters need.
type Config struct {
	ProviderType string
	BaseURL      string
	APIKey       string
	Model        string
	// Timeout bounds non-streaming calls. Streaming calls only use it for the
	// connect/header phase.
	Timeout time.Duration
	Client  *http.Client
}

func (c Config) client() *http.Client {
	if c.Client != nil {
		return c.Client
	}
	return http.DefaultClient
}

func (c Config) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return 5 * time.Minute
}

func (c Config) baseURL(def string) string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return def
}

// doJSON posts a JSON body and decodes a JSON response, mapping non-2xx
// statuses to *Error.
func doJSON(ctx context.Context, cfg Config, method, url string, headers map[string]string, body any, out any) error {
	ctx, cancel := context.WithTimeout(ctx, cfg.timeout())
	defer cancel()
	resp, err := doRequest(ctx, cfg, method, url, headers, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return &Error{Status: http.StatusBadGateway, Type: "upstream_error", Message: "read upstream response: " + err.Error()}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return upstreamError(resp.StatusCode, raw)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return &Error{Status: http.StatusBadGateway, Type: "upstream_error", Message: "decode upstream response: " + err.Error()}
	}
	return nil
}

// doStream posts a JSON body and returns the raw response for SSE reading.
func doStream(ctx context.Context, cfg Config, url string, headers map[string]string, body any) (*http.Response, error) {
	resp, err := doRequest(ctx, cfg, http.MethodPost, url, headers, body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, upstreamError(resp.StatusCode, raw)
	}
	return resp, nil
}

func doRequest(ctx context.Context, cfg Config, method, url string, headers map[string]string, body any) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("User-Agent", "ragmux/1.0")
	for k, v := range headers {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	resp, err := cfg.client().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, &Error{Status: http.StatusGatewayTimeout, Type: "timeout", Message: "upstream request cancelled or timed out"}
		}
		return nil, &Error{Status: http.StatusBadGateway, Type: "upstream_error", Message: Redact("upstream request failed: " + err.Error())}
	}
	return resp, nil
}

// upstreamError normalises any provider error body into *Error.
func upstreamError(status int, raw []byte) *Error {
	e := &Error{Status: status, Type: "upstream_error", Message: strings.TrimSpace(string(raw))}
	if e.Message == "" {
		e.Message = http.StatusText(status)
	}
	var env struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(raw, &env) == nil && len(env.Error) > 0 {
		var obj struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    any    `json:"code"`
		}
		if json.Unmarshal(env.Error, &obj) == nil && obj.Message != "" {
			e.Message = obj.Message
			if obj.Type != "" {
				e.Type = obj.Type
			}
			if obj.Code != nil {
				e.Code = fmt.Sprint(obj.Code)
			}
		} else {
			var s string
			if json.Unmarshal(env.Error, &s) == nil && s != "" {
				e.Message = s
			}
		}
	}
	if len(e.Message) > 2000 {
		e.Message = e.Message[:2000]
	}
	e.Message = Redact(e.Message)
	// Do not relay 5xx codes verbatim as our own; mark them as bad gateway.
	if status >= 500 {
		e.Status = http.StatusBadGateway
	}
	return e
}

func chatID() string {
	return "chatcmpl-" + randomID(24)
}
