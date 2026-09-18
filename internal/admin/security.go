package admin

import (
	"net/http"
	"time"

	"github.com/ragmux/ragmux/internal/auth"
)

// ---- login security (admin only) ----

// loginSecurity summarises the login_attempts table: failures in the last
// hour and day, and the username/address pairs the limiter currently locks
// out (its own rule, so the list matches what Check refuses). Without a
// limiter the counters are still reported and the lockout list is empty.
func (a *Admin) loginSecurity(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := time.Now()
	lastHour, err := a.Store.CountFailedLoginAttemptsSince(ctx, now.Add(-time.Hour))
	if err != nil {
		a.fail(w, err)
		return
	}
	lastDay, err := a.Store.CountFailedLoginAttemptsSince(ctx, now.Add(-24*time.Hour))
	if err != nil {
		a.fail(w, err)
		return
	}
	lockouts := []auth.Lockout{}
	if a.Limiter != nil {
		if lockouts, err = a.Limiter.ActiveLockouts(ctx); err != nil {
			a.fail(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"failed_last_hour": lastHour,
		"failed_last_24h":  lastDay,
		"active_lockouts":  lockouts,
	})
}
