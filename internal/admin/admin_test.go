package admin

import (
	"context"
	"strings"
	"testing"
)

func TestConnInputValidate(t *testing.T) {
	ctx := context.Background()
	strict := &Admin{}
	open := &Admin{AllowPrivateUpstreams: true}
	listed := &Admin{PrivateAllowlist: map[string]bool{"localhost": true}}
	base := connInput{Name: "n", ProviderType: "custom_openai", ModelName: "gpt-4o", BaseURL: "https://api.example.com/v1"}
	with := func(f func(*connInput)) connInput { c := base; f(&c); return c }

	cases := []struct {
		name string
		a    *Admin
		in   connInput
		want string // substring of the error; "" means valid
	}{
		{"ok", strict, base, ""},
		{"no scheme", strict, with(func(c *connInput) { c.BaseURL = "api.example.com" }), "http:// or https://"},
		{"ftp", strict, with(func(c *connInput) { c.BaseURL = "ftp://api.example.com" }), "http:// or https://"},
		{"userinfo", strict, with(func(c *connInput) { c.BaseURL = "https://u:p@api.example.com" }), "credentials"},
		{"query", strict, with(func(c *connInput) { c.BaseURL = "https://api.example.com/v1?x=1" }), "query string"},
		{"fragment", strict, with(func(c *connInput) { c.BaseURL = "https://api.example.com/v1#f" }), "query string"},
		{"no host", strict, with(func(c *connInput) { c.BaseURL = "http:///v1" }), "host"},
		{"loopback literal", strict, with(func(c *connInput) { c.BaseURL = "http://127.0.0.1:11434" }), "PRIVATE_UPSTREAM_ALLOWLIST"},
		{"metadata", strict, with(func(c *connInput) { c.BaseURL = "http://169.254.169.254/latest" }), "private or local"},
		{"localhost name", strict, with(func(c *connInput) { c.BaseURL = "http://localhost:11434" }), "private or local"},
		{"localhost allowlisted", listed, with(func(c *connInput) { c.BaseURL = "http://LOCALHOST:11434" }), ""},
		{"loopback allowed globally", open, with(func(c *connInput) { c.BaseURL = "http://127.0.0.1:11434" }), ""},
		{"empty base_url for openai", strict, with(func(c *connInput) { c.ProviderType = "openai"; c.BaseURL = "" }), ""},
		{"empty base_url for custom", strict, with(func(c *connInput) { c.BaseURL = "" }), "required"},
		{"model slash prefix", strict, with(func(c *connInput) { c.ModelName = "/x" }), "model_name"},
		{"model dotdot", strict, with(func(c *connInput) { c.ModelName = "a/../b" }), "model_name"},
		{"model space", strict, with(func(c *connInput) { c.ModelName = "a b" }), "model_name"},
		{"model too long", strict, with(func(c *connInput) { c.ModelName = strings.Repeat("a", 129) }), "model_name"},
		{"model ok", strict, with(func(c *connInput) { c.ModelName = "org/model-1.5:latest@v2_x" }), ""},
	}
	for _, c := range cases {
		err := c.in.validate(ctx, c.a)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: unexpected error %v", c.name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
}

func TestEffectiveLimitAndStoreQuota(t *testing.T) {
	cases := []struct{ store, instance, want int64 }{
		{0, 0, 0}, {10, 0, 10}, {0, 10, 10}, {5, 10, 5}, {10, 5, 5},
	}
	for _, c := range cases {
		if got := effectiveLimit(c.store, c.instance); got != c.want {
			t.Errorf("effectiveLimit(%d, %d) = %d, want %d", c.store, c.instance, got, c.want)
		}
	}
	q := &storeQuota{maxDocs: 2, docs: 1, maxBytes: 100, bytes: 60}
	if err := q.add(30); err != nil {
		t.Fatalf("first file should fit: %v", err)
	}
	if err := q.add(5); err == nil || !strings.Contains(err.Error(), "2 of 2 documents") {
		t.Errorf("document limit: %v", err)
	}
	q = &storeQuota{maxBytes: 100, bytes: 60}
	if err := q.add(41); err == nil || !strings.Contains(err.Error(), "60 of 100 bytes") {
		t.Errorf("byte limit: %v", err)
	}
	if err := (&storeQuota{}).add(1 << 40); err != nil {
		t.Errorf("unlimited: %v", err)
	}
}

func TestUpstreamPrivateLiterals(t *testing.T) {
	ctx := context.Background()
	for baseURL, want := range map[string]bool{
		"":                          false, // provider default URL
		"http://localhost:11434":    true,
		"http://127.0.0.1:8000/v1":  true,
		"http://[::1]:8000/v1":      true,
		"http://172.16.5.5/v1":      true,
		"http://169.254.169.254/v1": true,
		"https://8.8.8.8/v1":        false,
	} {
		got := upstreamPrivate(ctx, baseURL)
		if got == nil || *got != want {
			t.Errorf("upstreamPrivate(%q) = %v, want %v", baseURL, got, want)
		}
	}
	if got := upstreamPrivate(ctx, "://bad"); got != nil {
		t.Errorf("unparseable url should be unknown, got %v", *got)
	}
}
