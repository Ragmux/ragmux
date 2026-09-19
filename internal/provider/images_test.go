package provider

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestParseContent(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    []contentPart
		wantErr bool
	}{
		{name: "bare string", raw: `"hello"`, want: []contentPart{{Type: "text", Text: "hello"}}},
		{name: "text part", raw: `[{"type":"text","text":"a"}]`, want: []contentPart{{Type: "text", Text: "a"}}},
		{
			// OpenAI reads a part without a type as text; before the shared
			// parser only the Ollama adapter did.
			name: "part without a type", raw: `[{"text":"a"}]`,
			want: []contentPart{{Type: "text", Text: "a"}},
		},
		{
			name: "data url", raw: `[{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"}}]`,
			want: []contentPart{{Type: "image_url", Image: imageRef{MediaType: "image/png", Base64: "QUJD"}}},
		},
		{
			name: "remote url", raw: `[{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]`,
			want: []contentPart{{Type: "image_url", Image: imageRef{URL: "https://example.com/a.png"}}},
		},
		{
			name: "mixed", raw: `[{"type":"text","text":"what is this?"},{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,Zm9v"}}]`,
			want: []contentPart{{Type: "text", Text: "what is this?"}, {Type: "image_url", Image: imageRef{MediaType: "image/jpeg", Base64: "Zm9v"}}},
		},
		{name: "unknown part type", raw: `[{"type":"input_audio","input_audio":{"data":"x"}}]`, want: []contentPart{}},
		{name: "image part without a url", raw: `[{"type":"image_url","image_url":{}}]`, want: []contentPart{}},
		{name: "data url without a comma", raw: `[{"type":"image_url","image_url":{"url":"data:image/png;base64"}}]`, want: []contentPart{}},
		{name: "malformed", raw: `{"type":"text"}`, wantErr: true},
		{name: "empty", raw: ``, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseContent(json.RawMessage(tc.raw))
			if tc.wantErr {
				var pe *Error
				if err == nil {
					t.Fatalf("expected an error, got %+v", got)
				}
				if !errors.As(err, &pe) || pe.Status != 400 {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %d parts: %+v", len(got), got)
			}
			for i := range tc.want {
				if got[i].Type != tc.want[i].Type || got[i].Text != tc.want[i].Text || got[i].Image != tc.want[i].Image {
					t.Errorf("part %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestParseContentKeepsCacheControl(t *testing.T) {
	parts, err := parseContent(json.RawMessage(`[{"type":"text","text":"a","cache_control":{"type":"ephemeral"}}]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 1 || string(parts[0].CacheControl) != `{"type":"ephemeral"}` {
		t.Errorf("parts = %+v", parts)
	}
}
