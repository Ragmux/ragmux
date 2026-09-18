package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ragmux/ragmux/internal/testdb"
)

func TestMiddlewareCookieCSRF(t *testing.T) {
	ctx := context.Background()
	st := testdb.Open(t)
	u, err := st.CreateUser(ctx, "u", "h", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctx, u.ID, "tok", time.Hour); err != nil {
		t.Fatal(err)
	}
	svc := &Service{Store: st, TTL: time.Hour}
	h := svc.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if UserFrom(r.Context()) == nil {
			t.Error("user missing from context")
		}
		w.WriteHeader(http.StatusOK)
	}))
	srv := httptest.NewServer(h)
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	cases := []struct {
		name    string
		method  string
		cookie  bool
		bearer  bool
		headers map[string]string
		want    int
	}{
		{"cookie + foreign origin", "POST", true, false, map[string]string{"Origin": "https://evil.example"}, 403},
		{"cookie + cross-site fetch metadata", "POST", true, false, map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "http://" + host}, 403},
		{"cookie + same-site subdomain", "DELETE", true, false, map[string]string{"Sec-Fetch-Site": "same-site"}, 403},
		{"cookie + null origin", "PUT", true, false, map[string]string{"Origin": "null"}, 403},
		{"cookie + same-origin metadata", "POST", true, false, map[string]string{"Sec-Fetch-Site": "same-origin"}, 200},
		{"cookie + user navigation", "POST", true, false, map[string]string{"Sec-Fetch-Site": "none"}, 200},
		{"cookie + matching origin (old browser)", "POST", true, false, map[string]string{"Origin": "http://" + host}, 200},
		{"cookie + no browser headers (cookie jar client)", "POST", true, false, nil, 200},
		{"cookie GET from anywhere", "GET", true, false, map[string]string{"Origin": "https://evil.example"}, 200},
		{"bearer + foreign origin", "POST", false, true, map[string]string{"Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site"}, 200},
		{"bearer wins over a cookie", "POST", true, true, map[string]string{"Sec-Fetch-Site": "cross-site"}, 200},
		{"no credentials", "POST", false, false, map[string]string{"Sec-Fetch-Site": "same-origin"}, 401},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(tc.method, srv.URL+"/x", nil)
			if tc.cookie {
				req.AddCookie(&http.Cookie{Name: CookieName, Value: "tok"})
			}
			if tc.bearer {
				req.Header.Set("Authorization", "Bearer tok")
			}
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Errorf("status %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

func TestCookieSecureFlag(t *testing.T) {
	cases := []struct {
		name   string
		svc    Service
		tls    bool
		proto  string
		secure bool
	}{
		{"plain http", Service{}, false, "", false},
		{"SECURE_COOKIES", Service{Secure: true}, false, "", true},
		{"direct tls", Service{}, true, "", true},
		{"proxy says https but not trusted", Service{}, false, "https", false},
		{"trusted proxy says https", Service{TrustProxy: true}, false, "https", true},
		{"trusted proxy says http", Service{TrustProxy: true}, false, "http", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.svc.TTL = time.Hour
			req := httptest.NewRequest(http.MethodPost, "/", nil)
			if tc.tls {
				req = httptest.NewRequest(http.MethodPost, "https://x/", nil)
			}
			if tc.proto != "" {
				req.Header.Set("X-Forwarded-Proto", tc.proto)
			}
			rec := httptest.NewRecorder()
			tc.svc.SetCookie(rec, req, "tok")
			tc.svc.ClearCookie(rec, req)
			cookies := rec.Result().Cookies()
			if len(cookies) != 2 {
				t.Fatalf("cookies = %v", cookies)
			}
			for _, c := range cookies {
				if c.Secure != tc.secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
					t.Errorf("cookie %v: secure=%v want %v", c, c.Secure, tc.secure)
				}
			}
		})
	}
}
