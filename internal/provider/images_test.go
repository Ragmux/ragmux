package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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
		{name: "data url without a comma", raw: `[{"type":"image_url","image_url":{"url":"data:image/png;base64"}}]`, wantErr: true},
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

// A data: URL used to skip every check a fetched image goes through: the
// media type whitelist was applied only on the remote path, so
// "data:text/html;base64,…" arrived at the provider as an image, a
// "data:,payload" with neither a type nor base64 encoding did too, and each
// came back as an upstream 400 naming nothing the caller could act on.
func TestParseInlineImage(t *testing.T) {
	cases := []struct {
		name, url string
		want      imageRef
		wantErr   string
	}{
		{
			name: "png", url: "data:image/png;base64,QUJD",
			want: imageRef{MediaType: "image/png", Base64: "QUJD"},
		},
		{
			// The media type is a token, so its case says nothing.
			name: "media type is normalised", url: "data:IMAGE/JPEG;base64,QUJD",
			want: imageRef{MediaType: "image/jpeg", Base64: "QUJD"},
		},
		{
			// RFC 2397 puts ";base64" last among the parameters; the ones
			// before it are the media type's own.
			name: "parameters before base64", url: "data:image/webp;charset=binary;base64,QUJD",
			want: imageRef{MediaType: "image/webp", Base64: "QUJD"},
		},
		{name: "empty payload", url: "data:image/gif;base64,", want: imageRef{MediaType: "image/gif"}},
		{
			name: "html", url: "data:text/html;base64,PGh0bWw+",
			wantErr: `media type "text/html" is not supported`,
		},
		{
			name: "no media type and no encoding", url: "data:,plainpayload",
			wantErr: "must be a base64 data: URL",
		},
		{
			name: "percent-encoded rather than base64", url: "data:image/png,%89PNG",
			wantErr: "must be a base64 data: URL",
		},
		{name: "no comma", url: "data:image/png;base64", wantErr: "must be a base64 data: URL"},
		{name: "corrupt payload", url: "data:image/png;base64,!!!", wantErr: "not valid base64"},
		{
			name: "payload that is not padded", url: "data:image/png;base64,QUJDR",
			wantErr: "not valid base64",
		},
		{name: "remote url is left alone", url: "https://example.com/a.png",
			want: imageRef{URL: "https://example.com/a.png"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseImageURL(tc.url)
			if tc.wantErr != "" {
				var pe *Error
				if !errors.As(err, &pe) || pe.Status != http.StatusBadRequest ||
					!strings.Contains(pe.Message, tc.wantErr) {
					t.Fatalf("err = %v, want a 400 containing %q", err, tc.wantErr)
				}
				// The payload is the client's own bytes and may be megabytes
				// of them; nothing about it belongs in an error message.
				if _, payload, ok := strings.Cut(tc.url, ","); ok && payload != "" &&
					strings.Contains(pe.Message, payload) {
					t.Errorf("message echoes the payload: %s", pe.Message)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("ref = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A media type is unbounded client input, so it cannot go into an error
// message whole.
func TestParseInlineImageBoundsTheMediaTypeInTheMessage(t *testing.T) {
	long := strings.Repeat("ü", 4096)
	_, err := parseImageURL("data:" + long + ";base64,QUJD")
	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v", err)
	}
	if len(pe.Message) > 256 {
		t.Errorf("message is %d bytes long", len(pe.Message))
	}
}

// Nothing reaches the provider: the refusal happens while the request is
// being translated, before any adapter builds an upstream call.
//
// The OpenAI-compatible types are not in the list because they translate
// nothing -- the client's body is relayed verbatim and the upstream is the
// one that reads the image part, which is the whole point of that path.
func TestInlineImageNeverReachesProvider(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	for _, providerType := range []string{"gemini", "ollama", "anthropic"} {
		for _, url := range []string{
			"data:text/html;base64,PGh0bWw+", // media type off the whitelist
			"data:image/png,QUJD",            // no ;base64
			"data:image/png;base64,!!!",      // payload that is not base64
		} {
			t.Run(providerType+" "+url, func(t *testing.T) {
				p, err := New(Config{ProviderType: providerType, BaseURL: srv.URL, APIKey: "k", Model: "m"})
				if err != nil {
					t.Fatal(err)
				}
				body := fmt.Sprintf(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":%q}}]}]}`, url)
				_, err = p.Chat(context.Background(), parseReq(t, body))
				var pe *Error
				if !errors.As(err, &pe) || pe.Status != http.StatusBadRequest {
					t.Fatalf("err = %v, want a 400 from the gateway", err)
				}
			})
		}
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Errorf("%d requests reached the provider", n)
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
	f := &ImageFetcher{Client: http.DefaultClient, Cache: NewImageCache(16, 0, time.Minute)}
	if f.maxBytes() != 8<<20 || f.timeout() != 10*time.Second || f.maxPerRequest() != 8 ||
		f.maxConcurrent() != 4 {
		t.Errorf("defaults = %d %v %d %d", f.maxBytes(), f.timeout(), f.maxPerRequest(), f.maxConcurrent())
	}
	if f.Cache.maxEntries != 16 || f.Cache.ttl != time.Minute || f.Cache.maxBytes != 64<<20 {
		t.Errorf("cache = %+v", f.Cache)
	}
	// The byte ceiling is a caller's decision now: it has to be able to
	// follow IMAGE_FETCH_MAX_MB, which a fixed 64 MiB could not.
	if big := NewImageCache(16, 256<<20, time.Minute); big.maxBytes != 256<<20 {
		t.Errorf("cache ceiling = %d", big.maxBytes)
	}
	set := &ImageFetcher{MaxBytes: 1 << 20, Timeout: time.Second, MaxPerRequest: 2, MaxConcurrent: 3}
	if set.maxBytes() != 1<<20 || set.timeout() != time.Second || set.maxPerRequest() != 2 ||
		set.maxConcurrent() != 3 {
		t.Errorf("explicit settings ignored: %+v", set)
	}
}

// The per-request ceiling was measured by hand and never held by a test: only
// its getter was. It is what keeps one chat request from fanning out into a
// burst of outbound GETs.
func TestImageBudget(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("bytes"))
	}))
	defer srv.Close()

	cfg := Config{Images: &ImageFetcher{Client: srv.Client(), MaxPerRequest: 2}}
	b := cfg.imageBudget(context.Background())
	for i := 0; i < 2; i++ {
		got, err := b.inline(imageRef{URL: fmt.Sprintf("%s/%d.png", srv.URL, i)})
		if err != nil {
			t.Fatalf("fetch %d: %v", i, err)
		}
		if got.Base64 == "" || got.URL != "" || got.MediaType != "image/png" {
			t.Errorf("fetch %d = %+v", i, got)
		}
	}
	// The third is over the ceiling, and is refused before anything goes out.
	_, err := b.inline(imageRef{URL: srv.URL + "/3.png"})
	var pe *Error
	if !errors.As(err, &pe) || pe.Status != http.StatusBadRequest ||
		!strings.Contains(pe.Message, "at most 2 remote images") {
		t.Fatalf("err = %v", err)
	}
	if n := atomic.LoadInt32(&hits); n != 2 {
		t.Errorf("%d fetches went out, want 2", n)
	}

	// The ceiling counts fetches, not image parts: a conversation carrying
	// nothing but inline data: images never touches it.
	inline := imageRef{MediaType: "image/png", Base64: "QUJD"}
	spent := cfg.imageBudget(context.Background())
	for i := 0; i < 5; i++ {
		if got, err := spent.inline(inline); err != nil || got != inline {
			t.Fatalf("inline image %d: %+v %v", i, got, err)
		}
	}
	// A gateway with fetching switched off, and a request that never built a
	// budget at all, both pass the reference through for the adapter's own
	// policy to answer.
	if got, err := (Config{}).imageBudget(context.Background()).inline(inline); err != nil || got != inline {
		t.Errorf("fetching disabled: %+v %v", got, err)
	}
	var none *imageBudget
	remote := imageRef{URL: "https://example.com/a.png"}
	if got, err := none.inline(remote); err != nil || got != remote {
		t.Errorf("nil budget: %+v %v", got, err)
	}
}

// MaxPerRequest bounds one request and nothing across them: a key holder who
// sends many at once turns the gateway into a GET amplifier aimed from its own
// address at whatever host the requests name. This is the ceiling that stops
// that, so it has to hold when the requests are concurrent.
func TestImageFetcherConcurrencyCeiling(t *testing.T) {
	const (
		limit = 3
		total = 9
	)
	started := make(chan struct{}, total)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		started <- struct{}{}
		<-release
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("bytes"))
	}))
	defer srv.Close()

	f := &ImageFetcher{Client: srv.Client(), MaxConcurrent: limit}
	done := make(chan error, total)
	for i := 0; i < total; i++ {
		go func(i int) {
			_, _, err := f.Fetch(context.Background(), fmt.Sprintf("%s/%d.png", srv.URL, i))
			done <- err
		}(i)
	}
	for i := 0; i < limit; i++ {
		<-started
	}
	// Every handler is parked, so the remaining fetches can only be waiting
	// for a slot. If a fourth request arrives, the ceiling does not hold.
	select {
	case <-started:
		t.Fatal("more fetches went out at once than MaxConcurrent allows")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	for i := 0; i < total; i++ {
		if err := <-done; err != nil {
			t.Errorf("fetch %d: %v", i, err)
		}
	}
}

// A fetch that cannot get a slot in time is the gateway running out, not the
// client getting something wrong and not the image host failing, so it is
// neither a 400 nor a relayed upstream error.
func TestImageFetcherQueueSaturation(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		started <- struct{}{}
		<-release
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("bytes"))
	}))
	defer srv.Close()
	defer close(release)

	f := &ImageFetcher{Client: srv.Client(), MaxConcurrent: 1}
	go func() { _, _, _ = f.Fetch(context.Background(), srv.URL+"/held.png") }()
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, _, err := f.Fetch(ctx, srv.URL+"/queued.png")
	var pe *Error
	if !errors.As(err, &pe) || pe.Status != http.StatusServiceUnavailable {
		t.Fatalf("err = %v, want a 503", err)
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

// A fetcher without a client must refuse rather than reach for
// http.DefaultClient: this is the one place the gateway follows a URL a
// client chose, and an unguarded transport here would reach loopback and
// private addresses.
func TestImageFetcherWithoutClientRefuses(t *testing.T) {
	f := &ImageFetcher{}
	_, _, err := f.Fetch(context.Background(), "http://127.0.0.1/secret.png")
	if err == nil {
		t.Fatal("a fetcher with no client fetched anyway")
	}
	var perr *Error
	if !errors.As(err, &perr) || perr.Status != 400 {
		t.Fatalf("err = %v, want a 400 provider error", err)
	}
}
