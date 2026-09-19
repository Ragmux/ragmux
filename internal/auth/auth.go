// Package auth handles dashboard login sessions and password hashing.
package auth

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/ragmux/ragmux/internal/store"
)

// CookieName is the dashboard session cookie.
const CookieName = "ragmux_session"

type ctxKey struct{}
type sessionKey struct{}

// Session describes the session a request authenticated with.
type Session struct {
	// ExpiresAt is when the session stops resolving. It is the zero time for
	// a management key without an expiry.
	ExpiresAt time.Time
	// Bearer is true when the token came in the Authorization header
	// rather than the cookie.
	Bearer bool
	// KeyID and Scopes are set when the request authenticated with an
	// "sk-mgmt-" key rather than a dashboard session.
	KeyID  *int64
	Scopes []string
}

// IsKey reports whether the request authenticated with an api key.
func (s *Session) IsKey() bool { return s != nil && s.KeyID != nil }

// Kind names how the request authenticated, as /admin/api/me reports it.
func (s *Session) Kind() string {
	if s.IsKey() {
		return "api_key"
	}
	return "session"
}

// HashPassword bcrypt-hashes a password.
func HashPassword(pw string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	return string(h), err
}

// CheckPassword verifies a password against its hash.
func CheckPassword(hash, pw string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

// Service issues and validates sessions.
type Service struct {
	Store *store.Store
	TTL   time.Duration
	// Secure marks the cookie Secure unconditionally (SECURE_COOKIES). The
	// flag is also set when the request itself arrived over TLS, or, with
	// TrustProxy, when the proxy reports X-Forwarded-Proto: https.
	Secure bool
	// TrustProxy mirrors TRUST_PROXY_HEADERS for the X-Forwarded-Proto check.
	TrustProxy bool
}

// ErrInvalidCredentials is returned by Login on a bad username/password.
var ErrInvalidCredentials = errors.New("invalid credentials")

// Login checks credentials and creates a session token.
func (s *Service) Login(ctx context.Context, username, password string) (*store.User, string, error) {
	u, err := s.Store.GetUserByUsername(ctx, username)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Burn similar time to a real comparison.
			_ = bcrypt.CompareHashAndPassword([]byte("$2a$10$7EqJtq98hPqEX7fNZaFWoOhi5XKnb6.7YyN5m9uI0dKJmB9Zxy1Pa"), []byte(password))
			return nil, "", ErrInvalidCredentials
		}
		return nil, "", err
	}
	if !CheckPassword(u.PasswordHash, password) || !u.IsActive {
		return nil, "", ErrInvalidCredentials
	}
	tok, err := s.NewSession(ctx, u.ID)
	if err != nil {
		return nil, "", err
	}
	return u, tok, nil
}

// NewSession issues a session token for a user whose identity the caller
// has already established (a login, or the first-run setup).
func (s *Service) NewSession(ctx context.Context, userID int64) (string, error) {
	tok, err := store.GenerateSessionToken()
	if err != nil {
		return "", err
	}
	if err := s.Store.CreateSession(ctx, userID, tok, s.TTL); err != nil {
		return "", err
	}
	return tok, nil
}

// Logout revokes the session in the request.
func (s *Service) Logout(ctx context.Context, r *http.Request) error {
	if tok := TokenFromRequest(r); tok != "" {
		return s.Store.DeleteSession(ctx, tok)
	}
	return nil
}

// SetCookie writes the session cookie for the request's connection.
func (s *Service) SetCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure is derived from the connection so plain-HTTP deployments keep working
		Name: CookieName, Value: token, Path: "/", HttpOnly: true, Secure: s.secure(r),
		SameSite: http.SameSiteLaxMode, MaxAge: int(s.TTL.Seconds()),
	})
}

// ClearCookie expires the session cookie.
func (s *Service) ClearCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure is derived from the connection so plain-HTTP deployments keep working
		Name: CookieName, Value: "", Path: "/", HttpOnly: true, Secure: s.secure(r), SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
}

// secure reports whether the cookie for this request should carry Secure.
func (s *Service) secure(r *http.Request) bool {
	if s.Secure || r.TLS != nil {
		return true
	}
	return s.TrustProxy && strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https")
}

// TokenFromRequest returns the session token the request carries: the bearer
// header wins over the cookie.
func TokenFromRequest(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	if c, err := r.Cookie(CookieName); err == nil {
		return c.Value
	}
	return ""
}

// IsBearer reports whether the request authenticates with an Authorization
// header rather than the session cookie. Bearer requests cannot be forged by
// a foreign page, so the cross-site checks do not apply to them.
func IsBearer(r *http.Request) bool {
	return strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ")
}

// Middleware rejects requests without a valid session or management key, and
// cookie-authenticated state changes that a foreign origin initiated.
func (s *Service) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := TokenFromRequest(r)
		if tok == "" {
			unauthorized(w)
			return
		}
		if !IsBearer(r) && !SameOriginOK(r) {
			forbiddenCrossSite(w)
			return
		}
		// A management key is only ever accepted from the Authorization
		// header, and a key pasted into the cookie is refused before it is
		// even looked up. That is what keeps the bearer exemption above
		// sound for keys: a browser cannot attach an Authorization header
		// cross-origin without a CORS preflight, and cors() only answers
		// preflights for the origins in CORS_ORIGINS (empty by default),
		// while it would attach a cookie to any cross-site form post.
		if strings.HasPrefix(tok, store.ManagementKeyPrefix) {
			if !IsBearer(r) {
				unauthorized(w)
				return
			}
			k, u, err := s.Store.ResolveManagementKey(r.Context(), tok)
			if err != nil {
				unauthorized(w)
				return
			}
			var expires time.Time
			if k.ExpiresAt != nil {
				expires = *k.ExpiresAt
			}
			ctx := ContextWithUser(r.Context(), u)
			ctx = ContextWithSession(ctx, &Session{ExpiresAt: expires, Bearer: true, KeyID: &k.ID, Scopes: k.Scopes})
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		u, expires, err := s.Store.UserAndExpiryBySession(r.Context(), tok)
		if err != nil {
			unauthorized(w)
			return
		}
		ctx := ContextWithUser(r.Context(), u)
		ctx = ContextWithSession(ctx, &Session{ExpiresAt: expires, Bearer: IsBearer(r)})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// SameOriginOK is the CSRF check for cookie-authenticated requests. Reads
// always pass. For state-changing methods the browser's Sec-Fetch-Site must
// say same-origin (or none: typed URL, bookmark); "same-site" (a sibling
// subdomain) and "cross-site" are refused. Older browsers do not send
// Sec-Fetch-Site but always send Origin on cross-origin POSTs, so its host
// must then match the request host. Requests with neither header come from
// non-browser clients using a cookie jar and are allowed.
func SameOriginOK(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		return site == "same-origin" || site == "none"
	}
	origin := r.Header.Get("Origin")
	if origin == "" || origin == "null" {
		return origin == ""
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// ContextWithUser attaches a user the way Middleware does (for tests and
// internal callers).
func ContextWithUser(ctx context.Context, u *store.User) context.Context {
	return context.WithValue(ctx, ctxKey{}, u)
}

// UserFrom returns the authenticated user, if any.
func UserFrom(ctx context.Context) *store.User {
	u, _ := ctx.Value(ctxKey{}).(*store.User)
	return u
}

// ContextWithSession attaches the session description the way Middleware
// does.
func ContextWithSession(ctx context.Context, se *Session) context.Context {
	return context.WithValue(ctx, sessionKey{}, se)
}

// SessionFrom returns the session the request authenticated with, if any.
func SessionFrom(ctx context.Context) *Session {
	se, _ := ctx.Value(sessionKey{}).(*Session)
	return se
}

func forbiddenCrossSite(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(`{"error":{"message":"cross-site request rejected","type":"forbidden"}}`))
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":{"message":"authentication required","type":"unauthorized"}}`))
}
