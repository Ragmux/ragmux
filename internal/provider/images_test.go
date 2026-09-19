package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ragmux/ragmux/internal/netguard"
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

func TestImageFetcher(t *testing.T) {
	png := strings.Repeat("P", 1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s", r.Method)
		}
		switch r.URL.Path {
		case "/ok.png":
			w.Header().Set("Content-Type", "image/png; charset=binary")
			_, _ = w.Write([]byte(png))
		case "/chunked.png":
			// No Content-Length at all: the header check is only an early
			// exit, so the read itself has to hold the limit or a host that
			// declares nothing could spend our memory.
			w.Header().Set("Content-Type", "image/png")
			for i := 0; i < 40; i++ {
				_, _ = w.Write([]byte(strings.Repeat("X", 1024)))
				w.(http.Flusher).Flush()
			}
		case "/huge.png":
			w.Header().Set("Content-Type", "image/png")
			w.Header().Set("Content-Length", "1048576")
			_, _ = w.Write([]byte(strings.Repeat("X", 1<<20)))
		case "/page.html":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html>not an image</html>"))
		case "/away":
			http.Redirect(w, r, "http://elsewhere.example.com/a.png", http.StatusFound)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	// The same redirect policy the adapters get; a cross-host bounce is how an
	// image URL would otherwise reach a host it was not allowed to.
	client := &http.Client{Transport: srv.Client().Transport, CheckRedirect: netguard.CheckRedirect(3)}
	f := &ImageFetcher{Client: client, MaxBytes: 16 << 10}

	t.Run("png", func(t *testing.T) {
		mt, data, err := f.Fetch(context.Background(), srv.URL+"/ok.png")
		if err != nil {
			t.Fatal(err)
		}
		if mt != "image/png" || data != base64.StdEncoding.EncodeToString([]byte(png)) {
			t.Errorf("media type = %q, %d bytes of data", mt, len(data))
		}
	})
	for _, tc := range []struct {
		name, path, want string
	}{
		{"undeclared length", "/chunked.png", "larger than"},
		{"declared too large", "/huge.png", "larger than"},
		{"not an image", "/page.html", `content type "text/html"`},
		{"missing", "/nope.png", "image URL returned 404"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := f.Fetch(context.Background(), srv.URL+tc.path)
			var pe *Error
			if !errors.As(err, &pe) || pe.Status != 400 || !strings.Contains(pe.Message, tc.want) {
				t.Fatalf("err = %v", err)
			}
			// The message names the host, never the URL the client sent.
			if strings.Contains(pe.Message, tc.path) {
				t.Errorf("message leaks the path: %s", pe.Message)
			}
		})
	}
	t.Run("cross-host redirect refused", func(t *testing.T) {
		_, _, err := f.Fetch(context.Background(), srv.URL+"/away")
		var pe *Error
		if !errors.As(err, &pe) || !strings.Contains(pe.Message, "redirect rejected") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("scheme", func(t *testing.T) {
		for _, u := range []string{"file:///etc/passwd", "ftp://example.com/a.png", "notaurl"} {
			_, _, err := f.Fetch(context.Background(), u)
			var pe *Error
			if !errors.As(err, &pe) || pe.Status != 400 {
				t.Errorf("%s: err = %v", u, err)
			}
		}
	})
}

func TestImageFetcherCache(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "image/png")
		if r.URL.Path == "/nostore.png" {
			w.Header().Set("Cache-Control", "no-store, max-age=0")
		}
		_, _ = w.Write([]byte("bytes"))
	}))
	defer srv.Close()
	f := &ImageFetcher{Client: srv.Client(), Cache: newImageCache(8, 1<<20, time.Minute)}

	if _, _, err := f.Fetch(context.Background(), srv.URL+"/a.png"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.Fetch(context.Background(), srv.URL+"/a.png"); err != nil {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Errorf("the second fetch should have been served from the cache: %d upstream hits", hits)
	}
	// A response that says not to store it is fetched again every time.
	for i := 0; i < 2; i++ {
		if _, _, err := f.Fetch(context.Background(), srv.URL+"/nostore.png"); err != nil {
			t.Fatal(err)
		}
	}
	if hits != 3 {
		t.Errorf("no-store responses must not be cached: %d upstream hits", hits)
	}
}

// The exported surface main.go builds a fetcher from, so a wiring change in
// the binary cannot silently lose a setting.
func TestImageFetcherDefaults(t *testing.T) {
	f := &ImageFetcher{Client: http.DefaultClient, Cache: NewImageCache(16, time.Minute)}
	if f.maxBytes() != 8<<20 || f.timeout() != 10*time.Second || f.maxPerRequest() != 8 {
		t.Errorf("defaults = %d %v %d", f.maxBytes(), f.timeout(), f.maxPerRequest())
	}
	if f.Cache.maxEntries != 16 || f.Cache.ttl != time.Minute || f.Cache.maxBytes != 64<<20 {
		t.Errorf("cache = %+v", f.Cache)
	}
	set := &ImageFetcher{MaxBytes: 1 << 20, Timeout: time.Second, MaxPerRequest: 2}
	if set.maxBytes() != 1<<20 || set.timeout() != time.Second || set.maxPerRequest() != 2 {
		t.Errorf("explicit settings ignored: %+v", set)
	}
}

func TestImageCache(t *testing.T) {
	c := newImageCache(2, 1<<20, time.Minute)
	c.put("a", "image/png", "aaa")
	c.put("b", "image/png", "bbb")
	if mt, data, ok := c.get("a"); !ok || mt != "image/png" || data != "aaa" {
		t.Errorf("get a = %q %q %v", mt, data, ok)
	}
	// "a" was just read, so the third entry evicts "b" instead.
	c.put("c", "image/png", "ccc")
	if _, _, ok := c.get("b"); ok {
		t.Error("b should have been evicted")
	}
	if _, _, ok := c.get("a"); !ok {
		t.Error("a was used more recently than b")
	}
	// The byte ceiling evicts as well, whatever the entry count says.
	small := newImageCache(8, 6, time.Minute)
	small.put("a", "image/png", "aaa")
	small.put("b", "image/png", "bbb")
	small.put("c", "image/png", "ccc")
	if small.order.Len() != 2 || small.bytes != 6 {
		t.Errorf("entries = %d, bytes = %d", small.order.Len(), small.bytes)
	}
	expiring := newImageCache(8, 1<<20, time.Millisecond)
	expiring.put("a", "image/png", "aaa")
	time.Sleep(5 * time.Millisecond)
	if _, _, ok := expiring.get("a"); ok {
		t.Error("the entry should have expired")
	}
	if len(expiring.entries) != 0 {
		t.Errorf("an expired entry should be dropped: %d left", len(expiring.entries))
	}
	// A nil cache is the "caching disabled" case and must stay usable.
	var off *imageCache
	off.put("a", "image/png", "aaa")
	if _, _, ok := off.get("a"); ok {
		t.Error("a nil cache never hits")
	}
}
