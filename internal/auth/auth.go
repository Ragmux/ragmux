// Package auth handles dashboard login sessions and password hashing.
package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/ragmux/ragmux/internal/store"
)

// CookieName is the dashboard session cookie.
const CookieName = "ragmux_session"

type ctxKey struct{}

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
	// Secure marks the cookie Secure; enable behind TLS.
	Secure bool
}

// ErrInvalidCredentials is returned by Login on a bad username/password.
var ErrInvalidCredentials = errors.New("invalid credentials")

// Login checks credentials and creates a session token.
func (s *Service) Login(ctx context.Context, username, password string) (*store.User, string, error) {
	u, err := s.Store.GetUserByUsername(ctx, username)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Burn similar time to a real comparison.
			bcrypt.CompareHashAndPassword([]byte("$2a$10$7EqJtq98hPqEX7fNZaFWoOhi5XKnb6.7YyN5m9uI0dKJmB9Zxy1Pa"), []byte(password))
			return nil, "", ErrInvalidCredentials
		}
		return nil, "", err
	}
	if !CheckPassword(u.PasswordHash, password) || !u.IsActive {
		return nil, "", ErrInvalidCredentials
	}
	tok, err := store.GenerateSessionToken()
	if err != nil {
		return nil, "", err
	}
	if err := s.Store.CreateSession(ctx, u.ID, tok, s.TTL); err != nil {
		return nil, "", err
	}
	return u, tok, nil
}

// Logout revokes the session in the request.
func (s *Service) Logout(ctx context.Context, r *http.Request) error {
	if tok := tokenFromRequest(r); tok != "" {
		return s.Store.DeleteSession(ctx, tok)
	}
	return nil
}

// SetCookie writes the session cookie.
func (s *Service) SetCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name: CookieName, Value: token, Path: "/", HttpOnly: true, Secure: s.Secure,
		SameSite: http.SameSiteLaxMode, MaxAge: int(s.TTL.Seconds()),
	})
}

// ClearCookie expires the session cookie.
func (s *Service) ClearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: "", Path: "/", HttpOnly: true, MaxAge: -1})
}

func tokenFromRequest(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	if c, err := r.Cookie(CookieName); err == nil {
		return c.Value
	}
	return ""
}

// Middleware rejects requests without a valid session.
func (s *Service) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := tokenFromRequest(r)
		if tok == "" {
			unauthorized(w)
			return
		}
		u, err := s.Store.UserBySession(r.Context(), tok)
		if err != nil {
			unauthorized(w)
			return
		}
		next.ServeHTTP(w, r.WithContext(ContextWithUser(r.Context(), u)))
	})
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

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	w.Write([]byte(`{"error":{"message":"authentication required","type":"unauthorized"}}`))
}
