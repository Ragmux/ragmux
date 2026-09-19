package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ragmux/ragmux/internal/store"
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

// A management key is a bearer credential, so it skips the cross-site checks
// exactly as a bearer session token does — but only from the Authorization
// header. In the cookie it is refused before it is even looked up, because a
// browser attaches cookies to cross-site requests on its own while an
// Authorization header needs a CORS preflight the gateway only answers for
// CORS_ORIGINS.
func TestManagementKeyIsBearerOnly(t *testing.T) {
	ctx := context.Background()
	st := testdb.Open(t)
	u, err := st.CreateUser(ctx, "u", "h", "admin")
	if err != nil {
		t.Fatal(err)
	}
	k, raw, err := st.CreateAPIKey(ctx, &store.APIKey{Kind: store.KindManagement, Name: "ops", UserID: u.ID,
		Scopes: []string{store.ScopeRead, store.ScopeWrite}})
	if err != nil {
		t.Fatal(err)
	}
	svc := &Service{Store: st, TTL: time.Hour}
	var sawKey *int64
	var sawScopes []string
	h := svc.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		se := SessionFrom(r.Context())
		sawKey, sawScopes = se.KeyID, se.Scopes
		if UserFrom(r.Context()) == nil || !se.IsKey() || se.Kind() != "api_key" {
			t.Errorf("principal = %+v", se)
		}
		w.WriteHeader(http.StatusOK)
	}))
	srv := httptest.NewServer(h)
	defer srv.Close()

	cases := []struct {
		name    string
		cookie  bool
		bearer  bool
		headers map[string]string
		want    int
	}{
		{"bearer + foreign origin", false, true, map[string]string{"Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site"}, 200},
		{"bearer + same origin", false, true, nil, 200},
		{"in the cookie, same origin", true, false, map[string]string{"Sec-Fetch-Site": "same-origin"}, 401},
		{"in the cookie, cross-site", true, false, map[string]string{"Sec-Fetch-Site": "cross-site"}, 403},
		{"cookie key plus a bearer key", true, true, map[string]string{"Sec-Fetch-Site": "cross-site"}, 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sawKey, sawScopes = nil, nil
			req, _ := http.NewRequest(http.MethodPost, srv.URL+"/x", nil)
			if tc.cookie {
				req.AddCookie(&http.Cookie{Name: CookieName, Value: raw})
			}
			if tc.bearer {
				req.Header.Set("Authorization", "Bearer "+raw)
			}
			for hk, v := range tc.headers {
				req.Header.Set(hk, v)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("status %d, want %d", resp.StatusCode, tc.want)
			}
			if tc.want == 200 && (sawKey == nil || *sawKey != k.ID || len(sawScopes) != 2) {
				t.Errorf("session = %v %v", sawKey, sawScopes)
			}
		})
	}

	// A revoked key stops resolving on the next request.
	if _, err := st.RevokeAPIKey(ctx, k.ID); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/x", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("revoked key: status %d", resp.StatusCode)
	}
}

func TestRequireScope(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	cases := []struct {
		name    string
		session *Session
		want    int
	}{
		{"no session description", nil, 200},
		{"cookie session is never narrowed", &Session{}, 200},
		{"key with the scope", &Session{KeyID: new(int64), Scopes: []string{store.ScopeRead, store.ScopeWrite}}, 200},
		{"key without the scope", &Session{KeyID: new(int64), Scopes: []string{store.ScopeRead}}, 403},
		{"key with no scopes", &Session{KeyID: new(int64)}, 403},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", nil)
			if tc.session != nil {
				req = req.WithContext(ContextWithSession(req.Context(), tc.session))
			}
			rec := httptest.NewRecorder()
			RequireScope(store.ScopeWrite)(ok).ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Errorf("status %d, want %d", rec.Code, tc.want)
			}
			if tc.want == 403 && !strings.Contains(rec.Body.String(), "insufficient_scope") {
				t.Errorf("body %q", rec.Body.String())
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
