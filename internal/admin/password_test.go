package admin

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ragmux/ragmux/internal/auth"
	"github.com/ragmux/ragmux/internal/testdb"
)

// elevenChars is one character short of the password policy's floor.
const elevenChars = "eleven-char"

// postJSON runs handler on a JSON POST and returns the status and the error
// message of the answer.
func postJSON(t *testing.T, handler http.HandlerFunc, body map[string]any) (int, string) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(raw)))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	if !serve(t, handler, w, r) {
		return 0, ""
	}
	var out struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out.Error.Message
}

// serve runs handler and turns a panic into a test failure. The user
// management test drives handlers on an Admin without a store or a session
// user; if a handler ever reaches for either before validating the password,
// this reports that ordering change instead of crashing the package's tests.
func serve(t *testing.T, handler http.HandlerFunc, w http.ResponseWriter, r *http.Request) (ok bool) {
	t.Helper()
	defer func() {
		if p := recover(); p != nil {
			t.Errorf("handler panicked before rejecting the password (does it touch the store or the session user before validating?): %v", p)
			ok = false
		}
	}()
	handler(w, r)
	return true
}

// TestPasswordPolicyUserManagement checks that every dashboard path that sets
// a password refuses one character short of the policy with the policy's own
// message. Validation runs before any database work, so no store is needed;
// serve turns a violation of that ordering into a readable failure.
func TestPasswordPolicyUserManagement(t *testing.T) {
	if len(elevenChars) != 11 {
		t.Fatalf("elevenChars is %d characters", len(elevenChars))
	}
	a := &Admin{}
	want := auth.ErrPasswordTooShort.Error()
	cases := map[string]struct {
		handler http.HandlerFunc
		body    map[string]any
	}{
		"create user":     {a.createUser, map[string]any{"username": "u", "password": elevenChars}},
		"reset password":  {a.resetPassword, map[string]any{"new_password": elevenChars}},
		"change password": {a.changePassword, map[string]any{"current_password": "whatever", "new_password": elevenChars}},
	}
	for name, c := range cases {
		if st, msg := postJSON(t, c.handler, c.body); st != http.StatusBadRequest || msg != want {
			t.Errorf("%s with 11 characters: %d %q, want 400 %q", name, st, msg, want)
		}
	}
	if st, msg := postJSON(t, a.createUser, map[string]any{"username": "u", "password": strings.Repeat("p", 73)}); st != http.StatusBadRequest || msg != auth.ErrPasswordTooLong.Error() {
		t.Errorf("create user with 73 bytes: %d %q", st, msg)
	}
}

// TestPasswordPolicySetup checks the first-run setup refuses the same
// password with the same message, and that 12 characters are enough there.
func TestPasswordPolicySetup(t *testing.T) {
	st := testdb.Open(t)
	a := &Admin{
		Store: st,
		Auth:  &auth.Service{Store: st, TTL: time.Hour},
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if code, msg := postJSON(t, a.setup, map[string]any{"username": "root", "password": elevenChars}); code != http.StatusBadRequest || msg != auth.ErrPasswordTooShort.Error() {
		t.Errorf("setup with 11 characters: %d %q", code, msg)
	}
	if n, _ := st.CountUsers(context.Background()); n != 0 {
		t.Fatalf("a refused setup created %d user(s)", n)
	}
	if code, msg := postJSON(t, a.setup, map[string]any{"username": "root", "password": "twelve-chars"}); code != http.StatusCreated {
		t.Errorf("setup with 12 characters: %d %q", code, msg)
	}
}
