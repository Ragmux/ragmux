package admin

import (
	"errors"
	"net/http"
	"regexp"

	"github.com/ragmux/ragmux/internal/auth"
	"github.com/ragmux/ragmux/internal/store"
)

// ---- first-run setup ----
//
// A fresh database has no users. Instead of printing a generated password to
// the logs, the gateway lets whoever opens the dashboard first create the
// administrator. Both endpoints are unauthenticated: the status one only
// reveals whether setup is pending, and the creating one refuses as soon as
// any user exists, atomically (store.CreateFirstUser). ADMIN_PASSWORD still
// pre-creates the account for unattended installs, in which case setup is
// already complete when the first request arrives.

// setupUsernamePattern is stricter than createUser's: the first account is
// typed once and should be unambiguous in logs and audit rows.
var setupUsernamePattern = regexp.MustCompile(`^[a-z0-9._-]{3,64}$`)

// setupMinPasswordLen is longer than the 8 characters user management
// accepts: this password protects the whole installation from day one.
const setupMinPasswordLen = 12

// setupLimiterUser is the username under which failed setup attempts are
// recorded: the per-address login budget applies to them (CheckAddress)
// without touching any real account's counters.
const setupLimiterUser = ""

// setupStatus reports whether setup is pending. While it is, the answer also
// carries three facts the setup page shows so an operator can confirm which
// database the gateway is on -- the migration version, where SECRET_KEY came
// from and the database role -- plus has_connections and has_projects, which
// let the wizard resume at the right step after a reload. Nothing there
// identifies users or hosts: all five are booleans or instance-wide facts.
//
// Once a user exists the answer shrinks to needs_setup alone. The endpoint is
// unauthenticated by design, and after setup nobody needs the rest: an
// internet-facing install should not hand an anonymous curl the migration
// version, the database role, where the key came from and how far the
// install got. None of that is an opening on its own; together it is
// reconnaissance, and it costs nothing to stop publishing it.
func (a *Admin) setupStatus(w http.ResponseWriter, r *http.Request) {
	n, err := a.Store.CountUsers(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	if n != 0 {
		writeJSON(w, http.StatusOK, map[string]any{"needs_setup": false})
		return
	}
	info, err := a.Store.SetupInfo(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	conns, projects, err := a.Store.SetupProgress(r.Context())
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"needs_setup":        true,
		"migrations_version": info.MigrationsVersion,
		"secret_key_source":  a.Store.SecretKeySource,
		"database_role":      info.DatabaseRole,
		"has_connections":    conns,
		"has_projects":       projects,
	})
}

func (a *Admin) setup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	// Cheap refusal before any body parsing or limiter work once set up.
	if n, err := a.Store.CountUsers(ctx); err != nil {
		a.fail(w, err)
		return
	} else if n > 0 {
		writeErr(w, http.StatusConflict, store.ErrSetupDone.Error())
		return
	}
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Bearer   bool   `json:"bearer"`
	}
	if err := decodeLogin(r, &in); err != nil {
		badBody(w, err)
		return
	}
	ip := clientIP(r)
	if a.Limiter != nil {
		allowed, retryAfter, err := a.Limiter.CheckAddress(ctx, ip)
		if err != nil {
			a.fail(w, err)
			return
		}
		if !allowed {
			a.throttled(w, r, setupLimiterUser, retryAfter, false)
			return
		}
	}
	recordFailure := func() {
		if a.Limiter != nil {
			if err := a.Limiter.Record(ctx, setupLimiterUser, ip, false); err != nil {
				a.Log.Error("record setup attempt", "err", err)
			}
		}
	}
	if !setupUsernamePattern.MatchString(in.Username) {
		recordFailure()
		writeErr(w, http.StatusBadRequest, "username must be 3-64 characters of a-z, 0-9, '.', '_' or '-'")
		return
	}
	if len(in.Password) < setupMinPasswordLen {
		recordFailure()
		writeErr(w, http.StatusBadRequest, "password must be at least 12 characters")
		return
	}
	if len(in.Password) > maxPasswordLen {
		recordFailure()
		writeErr(w, http.StatusBadRequest, "password must be at most 72 bytes")
		return
	}
	hash, err := auth.HashPassword(in.Password)
	if err != nil {
		a.fail(w, err)
		return
	}
	u, err := a.Store.CreateFirstUser(ctx, in.Username, hash, string(auth.RoleAdmin))
	if err != nil {
		if errors.Is(err, store.ErrSetupDone) {
			recordFailure()
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		if store.IsUniqueViolation(err) {
			recordFailure()
			writeErr(w, http.StatusConflict, errUsernameTaken)
			return
		}
		a.fail(w, err)
		return
	}
	tok, err := a.Auth.NewSession(ctx, u.ID)
	if err != nil {
		a.fail(w, err)
		return
	}
	if err := a.Store.TouchLastLogin(ctx, u.ID); err != nil {
		a.Log.Error("touch last login", "err", err)
	}
	a.auditAs(r, u, "setup.complete", "user", &u.ID, map[string]any{"username": u.Username})
	a.Log.Info("first administrator created through setup", "username", u.Username, "ip", ip)
	a.Auth.SetCookie(w, r, tok)
	out := map[string]any{"user": u}
	if in.Bearer {
		out["token"] = tok
	}
	writeJSON(w, http.StatusCreated, out)
}
