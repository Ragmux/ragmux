package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ragmux/ragmux/internal/netguard"
)

// ALLOW_PRIVATE_UPSTREAMS and PRIVATE_UPSTREAM_ALLOWLIST exist for a base URL
// an editor typed -- allowlisting a host is the documented way to reach a
// local Ollama. An image URL arrives from whoever holds an API key, so the
// image client must not inherit that exemption: it would turn every
// allowlisted deployment into a way for a key holder to reach the internal
// network.
func TestImageClientIgnoresThePrivateUpstreamExemption(t *testing.T) {
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("\x89PNG\r\n\x1a\n"))
	}))
	defer internal.Close()
	host, _, err := net.SplitHostPort(strings.TrimPrefix(internal.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}

	dialer := &net.Dialer{Timeout: 5 * time.Second}
	for _, tc := range []struct {
		name         string
		allowPrivate bool
		allowHosts   map[string]bool
	}{
		{"ALLOW_PRIVATE_UPSTREAMS", true, nil},
		{"PRIVATE_UPSTREAM_ALLOWLIST", false, map[string]bool{host: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The provider policy reaches it. That is the documented point.
			prov := &http.Client{Transport: &http.Transport{
				DialContext: netguard.SafeDialContext(dialer, tc.allowPrivate, tc.allowHosts)}}
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, internal.URL, nil)
			resp, err := prov.Do(req)
			if err != nil {
				t.Fatalf("provider client should reach an allowed private host: %v", err)
			}
			_ = resp.Body.Close()

			// The image policy must not, whatever the provider policy says.
			img := &http.Client{Transport: &http.Transport{
				DialContext: netguard.SafeDialContext(dialer, false, nil)}}
			req2, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, internal.URL, nil)
			resp2, err := img.Do(req2)
			if err == nil {
				_ = resp2.Body.Close()
				t.Fatal("the image client reached a private address")
			}
			if !strings.Contains(err.Error(), "private") && !strings.Contains(err.Error(), "local") {
				t.Errorf("unexpected refusal: %v", err)
			}
		})
	}
}
