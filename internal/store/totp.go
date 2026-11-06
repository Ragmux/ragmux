package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// totpAADPrefix is the associated-data prefix of users.totp_secret_enc
// (ADR-037); the user id is appended. The column is in the sealed registry
// (sealed.go), so a key rotation reseals it with everything else.
const totpAADPrefix = "ragmux:user_totp:"

// The numbers of the second factor (ADR-037).
const (
	// MFALockFailures wrong codes within MFALockWindow lock the account's
	// second factor, whichever endpoint the codes were tried on.
	MFALockFailures = 5
	MFALockWindow   = 15 * time.Minute
	// LoginChallengeTTL is how long a challenge stays open after the password
	// step, and LoginChallengeMaxAttempts how many wrong codes close it.
	LoginChallengeTTL         = 5 * time.Minute
	LoginChallengeMaxAttempts = 5
	// SessionPasswordBudget is how many wrong passwords one session may try
	// against its own account (T10); the session ends on the last one.
	SessionPasswordBudget = 5
)

// The endpoints that verify a second factor, as the audit trail names them
// (details.via).
const (
	ViaLogin         = "login"
	ViaEnable        = "enable"
	ViaDisable       = "disable"
	ViaRecoveryCodes = "recovery_codes"
)

// Errors of the second factor. The gate returns them as they are so the
// handler picks the status code; none of them is a server fault.
var (
	// ErrMFABusy: another check of the same account is running (the row lock
	// was not free). The code was not looked at and nothing was counted.
	ErrMFABusy = errors.New("another second factor check is in progress")
	// ErrMFAWrongCode: the code did not verify. The failure was recorded.
	ErrMFAWrongCode = errors.New("second factor code is wrong")
	// ErrMFAInvalidChallenge: the challenge is missing, expired, spent, or the
	// account no longer has a second factor to answer it with.
	ErrMFAInvalidChallenge = errors.New("login challenge is invalid or expired")
	// ErrTOTPAlreadyEnabled, ErrTOTPNotSetUp and ErrTOTPNotEnabled: the
	// account is not in the state the operation needs.
	ErrTOTPAlreadyEnabled = errors.New("totp is already enabled")
	ErrTOTPNotSetUp       = errors.New("totp has not been set up")
	ErrTOTPNotEnabled     = errors.New("totp is not enabled")
)

// MFALockedError is returned while the account's second factor is locked:
// MFALockFailures wrong codes in the last MFALockWindow. A right code is
// refused too, and nothing is recorded.
type MFALockedError struct {
	// RetryAfter is how long until the oldest counted failure leaves the window.
	RetryAfter time.Duration
}

func (e *MFALockedError) Error() string {
	return fmt.Sprintf("second factor is locked, retry in %s", e.RetryAfter.Round(time.Second))
}

// TOTPStatus is the second-factor state of an account. Enabled implies
// HasSecret; a secret without Enabled is a setup nobody confirmed, and does not
// make the login two-step.
type TOTPStatus struct {
	HasSecret bool
	Enabled   bool
}

// GetTOTPStatus reads the second-factor state of a user.
func (s *Store) GetTOTPStatus(ctx context.Context, userID int64) (TOTPStatus, error) {
	var st TOTPStatus
	err := s.pool.QueryRow(ctx,
		"SELECT totp_secret_enc IS NOT NULL, totp_enabled_at IS NOT NULL FROM users WHERE id = $1", userID).
		Scan(&st.HasSecret, &st.Enabled)
	return st, scanErr(err)
}

// SetTOTPSecret stores a freshly generated secret, sealed, for an account that
// has not switched TOTP on. The write is conditional on that, in the statement
// itself, so an enabled account's secret is never replaced; it reports false
// when no row matched. A pending secret is replaced by the next call.
func (s *Store) SetTOTPSecret(ctx context.Context, userID int64, secret string) (bool, error) {
	enc, err := s.cipher.encrypt(secret, sealedAAD(totpAADPrefix, userID))
	if err != nil {
		return false, err
	}
	tag, err := s.pool.Exec(ctx, `UPDATE users SET totp_secret_enc = $1, totp_key_version = $2, totp_last_step = 0
		WHERE id = $3 AND totp_enabled_at IS NULL`, enc, keyVersionBound, userID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// EnableTOTPTx switches TOTP on inside tx. The write is conditional (a secret
// is there, TOTP is not on yet); ErrTOTPAlreadyEnabled when no row matched.
func EnableTOTPTx(ctx context.Context, tx pgx.Tx, userID int64) error {
	tag, err := tx.Exec(ctx, `UPDATE users SET totp_enabled_at = now()
		WHERE id = $1 AND totp_enabled_at IS NULL AND totp_secret_enc IS NOT NULL`, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrTOTPAlreadyEnabled
	}
	return nil
}

// ClearTOTPTx removes the second factor of a user inside tx: the secret, the
// enable time, the replay mark, the recovery codes and any open challenge.
// It leaves the failed-code rows alone.
func ClearTOTPTx(ctx context.Context, tx pgx.Tx, userID int64) error {
	for _, q := range []string{
		"UPDATE users SET totp_secret_enc = NULL, totp_enabled_at = NULL, totp_last_step = 0 WHERE id = $1",
		"DELETE FROM user_recovery_codes WHERE user_id = $1",
		"DELETE FROM login_challenges WHERE user_id = $1",
	} {
		if _, err := tx.Exec(ctx, q, userID); err != nil {
			return err
		}
	}
	return nil
}

// DisableUserTOTP removes an account's second factor from outside (an admin's
// reset or the CLI), together with its failed-code rows, and reports whether it
// had a secret. Unlike a user turning their own TOTP off it also forgets the
// failures: this is the way out of the lock.
func (s *Store) DisableUserTOTP(ctx context.Context, userID int64) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful commit
	var had bool
	if err := tx.QueryRow(ctx, "SELECT totp_secret_enc IS NOT NULL FROM users WHERE id = $1 FOR NO KEY UPDATE", userID).Scan(&had); err != nil {
		return false, scanErr(err)
	}
	if err := ClearTOTPTx(ctx, tx, userID); err != nil {
		return false, err
	}
	if err := clearMFAAttemptsTx(ctx, tx, userID); err != nil {
		return false, err
	}
	return had, tx.Commit(ctx)
}

// ClearUserMFAState forgets an account's failed second-factor codes and closes
// its open challenges. A password reset by someone else (an admin, the CLI)
// calls it: that is a way out of the MFA lock. A password change by the user
// does not clear the failures (DeleteUserLoginChallenges only), or rotating the
// password would reset the counter.
func (s *Store) ClearUserMFAState(ctx context.Context, userID int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful commit
	if err := clearMFAAttemptsTx(ctx, tx, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "DELETE FROM login_challenges WHERE user_id = $1", userID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// clearMFAAttemptsTx deletes the 'mfa' rows under the same spelling the gate
// writes them with: lower(users.username).
func clearMFAAttemptsTx(ctx context.Context, tx pgx.Tx, userID int64) error {
	_, err := tx.Exec(ctx, `DELETE FROM login_attempts WHERE stage = $2
		AND username = (SELECT lower(username) FROM users WHERE id = $1)`, userID, loginStageMFA)
	return err
}

// DeleteUserLoginChallenges closes every open challenge of a user.
func (s *Store) DeleteUserLoginChallenges(ctx context.Context, userID int64) error {
	_, err := s.pool.Exec(ctx, "DELETE FROM login_challenges WHERE user_id = $1", userID)
	return err
}

// ReplaceRecoveryCodesTx replaces all of a user's recovery codes with the given
// hashes inside tx; the old ones, spent or not, stop working.
func ReplaceRecoveryCodesTx(ctx context.Context, tx pgx.Tx, userID int64, hashes []string) error {
	if _, err := tx.Exec(ctx, "DELETE FROM user_recovery_codes WHERE user_id = $1", userID); err != nil {
		return err
	}
	for _, h := range hashes {
		if _, err := tx.Exec(ctx, "INSERT INTO user_recovery_codes (user_id, code_hash) VALUES ($1, $2)", userID, h); err != nil {
			return err
		}
	}
	return nil
}

// countRecoveryCodes returns how many unused recovery codes a user has, as
// seen by q (the gate asks inside its transaction).
func countRecoveryCodes(ctx context.Context, q rowQuerier, userID int64) (n int, err error) {
	err = q.QueryRow(ctx, "SELECT COUNT(*) FROM user_recovery_codes WHERE user_id = $1 AND used_at IS NULL", userID).Scan(&n)
	return n, err
}

// DeleteUserSessionsExceptTx is DeleteUserSessionsExcept inside tx.
func DeleteUserSessionsExceptTx(ctx context.Context, tx pgx.Tx, userID int64, tokenHash string) error {
	_, err := tx.Exec(ctx, "DELETE FROM sessions WHERE user_id = $1 AND token_hash <> $2", userID, tokenHash)
	return err
}

// CreateSessionIfNoTOTP stores a session for a one-step login only while the
// account still has no second factor, and reports whether it did. The status
// the login read a moment earlier may be stale: TOTP can be switched on in
// between, and its enable ends the account's other sessions in the same
// transaction. The statement therefore locks the user row FOR SHARE, which waits
// for an enable still in flight and then sees its result; a session written
// after the enable's commit can no longer slip past it.
func (s *Store) CreateSessionIfNoTOTP(ctx context.Context, userID int64, token string, ttl time.Duration) (bool, error) {
	tag, err := s.pool.Exec(ctx, `INSERT INTO sessions (user_id, token_hash, expires_at)
		SELECT id, $2, $3 FROM users WHERE id = $1 AND totp_enabled_at IS NULL FOR SHARE`,
		userID, HashToken(token), time.Now().UTC().Add(ttl))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ---- login challenges ----

// LoginChallenge is the state between the password and the second factor.
type LoginChallenge struct {
	ID        int64
	UserID    int64
	Bearer    bool
	Attempts  int
	ExpiresAt time.Time
}

// CreateLoginChallenge opens a challenge for a user whose password was right.
// Only the hash of the token is stored.
func (s *Store) CreateLoginChallenge(ctx context.Context, userID int64, token string, bearer bool, ip string, ttl time.Duration) (*LoginChallenge, error) {
	c := &LoginChallenge{UserID: userID, Bearer: bearer}
	err := s.pool.QueryRow(ctx, `INSERT INTO login_challenges (user_id, token_hash, bearer, expires_at, ip)
		VALUES ($1, $2, $3, now() + make_interval(secs => $4), $5) RETURNING id, expires_at`,
		userID, HashToken(token), bearer, ttl.Seconds(), ip).Scan(&c.ID, &c.ExpiresAt)
	if err != nil {
		return nil, err
	}
	c.ExpiresAt = c.ExpiresAt.UTC()
	return c, nil
}

// GetLoginChallenge looks an open challenge up by its token; ErrNotFound when
// there is none, it has expired or it was spent. The gate reads it again under
// the account's lock, so this answer is only a way to learn whose it is.
func (s *Store) GetLoginChallenge(ctx context.Context, token string) (*LoginChallenge, error) {
	c := &LoginChallenge{}
	err := s.pool.QueryRow(ctx, `SELECT id, user_id, bearer, attempts, expires_at FROM login_challenges
		WHERE token_hash = $1 AND expires_at > now()`, HashToken(token)).
		Scan(&c.ID, &c.UserID, &c.Bearer, &c.Attempts, &c.ExpiresAt)
	if err != nil {
		return nil, scanErr(err)
	}
	c.ExpiresAt = c.ExpiresAt.UTC()
	return c, nil
}

// ---- the second-factor gate ----

// TOTPCheck verifies a TOTP code against a secret, accepting only a step after
// lastStep, and returns the accepted step. auth.CheckTOTP is the one
// implementation; the store cannot import auth, so the caller hands it in.
type TOTPCheck func(secret, code string, lastStep int64) (step int64, ok bool)

// SecondFactor is one attempt to prove the second factor.
type SecondFactor struct {
	UserID int64
	// Via is ViaLogin, ViaEnable, ViaDisable or ViaRecoveryCodes. It decides
	// which account state the attempt needs and nothing else.
	Via string
	// ChallengeID is the login challenge the attempt answers; ViaLogin only.
	ChallengeID int64
	// IP is recorded with a failure.
	IP string
	// Exactly one proof: Code with Check for a TOTP code, or RecoveryHash (the
	// auth.HashRecoveryCode of the normalised code) for a recovery code. A
	// recovery code is taken by ViaLogin and ViaDisable only.
	Code         string
	Check        TOTPCheck
	RecoveryHash string
}

// SecondFactorResult says what a passed or failed attempt used up.
type SecondFactorResult struct {
	// UsedRecovery is true when a recovery code passed; RecoveryRemaining is
	// then how many unused codes are left.
	UsedRecovery      bool
	RecoveryRemaining int
	// AttemptsLeft is, after a wrong code on a login challenge, how many more
	// the challenge takes (0 once it was closed).
	AttemptsLeft int
}

// VerifySecondFactor is the one place a TOTP or recovery code is checked
// (ADR-037): login/mfa, enable, disable and recovery-codes all come through
// it, so the account has one lock whichever door the guesses come in at.
//
// Everything runs in one transaction:
//
//  1. The user row is locked FOR NO KEY UPDATE NOWAIT. When it is not free,
//     another check of the same account is running: ErrMFABusy, the code is not
//     looked at and nothing is counted (waiting would hold a pooled connection
//     per queued request). With the lock held, the challenge and the account's
//     second-factor state are read again, so a challenge another request just
//     spent, or a TOTP that was just turned off, is not honoured.
//  2. MFALockFailures wrong codes within MFALockWindow (login_attempts rows of
//     stage 'mfa', keyed on lower(users.username)) lock the gate:
//     *MFALockedError, even for a right code, and nothing is written.
//  3. The code is verified: a TOTP step after totp_last_step, or an unused
//     recovery code.
//  4. A wrong code writes its login_attempts row and counts against the
//     challenge (closing it on the last try); that commits, and the result is
//     ErrMFAWrongCode.
//  5. A right one marks the step or the code used, deletes the challenge and
//     runs onSuccess (the endpoint's own state change), all in the same commit.
//
// A failure of onSuccess rolls the whole attempt back, the code with it. The
// caller issues any session after this returns, so a failure there costs the
// user a code, not an open door. bcrypt and the audit entry stay outside.
func (s *Store) VerifySecondFactor(ctx context.Context, req SecondFactor, onSuccess func(pgx.Tx) error) (SecondFactorResult, error) {
	var res SecondFactorResult
	recovery := req.RecoveryHash != ""
	switch {
	case req.Via != ViaLogin && req.Via != ViaEnable && req.Via != ViaDisable && req.Via != ViaRecoveryCodes:
		return res, fmt.Errorf("store: unknown second factor via %q", req.Via)
	case recovery && (req.Via == ViaEnable || req.Via == ViaRecoveryCodes):
		return res, fmt.Errorf("store: a recovery code is not accepted by %s", req.Via)
	case recovery == (req.Code != ""), !recovery && req.Check == nil:
		return res, errors.New("store: a second factor attempt carries exactly one proof")
	case (req.Via == ViaLogin) != (req.ChallengeID != 0):
		return res, errors.New("store: only a login attempt answers a challenge, and it must")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return res, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful commit

	var (
		username  string
		secretEnc []byte
		keyVer    int16
		enabledAt *time.Time
		lastStep  int64
		active    bool
	)
	err = tx.QueryRow(ctx, `SELECT username, totp_secret_enc, totp_key_version, totp_enabled_at, totp_last_step, is_active
		FROM users WHERE id = $1 FOR NO KEY UPDATE NOWAIT`, req.UserID).
		Scan(&username, &secretEnc, &keyVer, &enabledAt, &lastStep, &active)
	if err != nil {
		if pgCode(err) == "55P03" { // lock_not_available
			return res, ErrMFABusy
		}
		return res, scanErr(err)
	}
	enabled := enabledAt != nil
	switch req.Via {
	case ViaLogin:
		if !enabled || !active {
			return res, ErrMFAInvalidChallenge
		}
	case ViaEnable:
		if enabled {
			return res, ErrTOTPAlreadyEnabled
		}
		if secretEnc == nil {
			return res, ErrTOTPNotSetUp
		}
	default:
		if !enabled {
			return res, ErrTOTPNotEnabled
		}
	}
	var attempts int16
	if req.Via == ViaLogin {
		err := tx.QueryRow(ctx, `SELECT attempts FROM login_challenges
			WHERE id = $1 AND user_id = $2 AND expires_at > now() FOR UPDATE`, req.ChallengeID, req.UserID).Scan(&attempts)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return res, ErrMFAInvalidChallenge
			}
			return res, err
		}
	}

	if locked, err := mfaLockedTx(ctx, tx, req.UserID); err != nil {
		return res, err
	} else if locked != nil {
		return res, locked
	}

	var (
		ok   bool
		step int64
	)
	if recovery {
		tag, err := tx.Exec(ctx, `UPDATE user_recovery_codes SET used_at = now()
			WHERE user_id = $1 AND code_hash = $2 AND used_at IS NULL`, req.UserID, req.RecoveryHash)
		if err != nil {
			return res, err
		}
		ok = tag.RowsAffected() == 1
	} else {
		if keyVer != keyVersionBound {
			return res, fmt.Errorf("store: user %d: unsupported totp key version %d", req.UserID, keyVer)
		}
		secret, err := s.cipher.decrypt(secretEnc, sealedAAD(totpAADPrefix, req.UserID))
		if err != nil {
			return res, fmt.Errorf("store: open the totp secret of user %d: %w", req.UserID, err)
		}
		step, ok = req.Check(secret, req.Code, lastStep)
	}

	if !ok {
		// lower(): the one spelling of login_attempts.username (T11), and the
		// one the lock above and the exits count and delete with.
		if _, err := tx.Exec(ctx, `INSERT INTO login_attempts (username, ip, success, stage)
			VALUES (lower($1), $2, false, $3)`, username, req.IP, loginStageMFA); err != nil {
			return res, err
		}
		if req.Via == ViaLogin {
			if err := tx.QueryRow(ctx, "UPDATE login_challenges SET attempts = attempts + 1 WHERE id = $1 RETURNING attempts",
				req.ChallengeID).Scan(&attempts); err != nil {
				return res, err
			}
			res.AttemptsLeft = max(LoginChallengeMaxAttempts-int(attempts), 0)
			if res.AttemptsLeft == 0 {
				if _, err := tx.Exec(ctx, "DELETE FROM login_challenges WHERE id = $1", req.ChallengeID); err != nil {
					return res, err
				}
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return res, err
		}
		return res, ErrMFAWrongCode
	}

	if !recovery {
		if _, err := tx.Exec(ctx, "UPDATE users SET totp_last_step = $2 WHERE id = $1", req.UserID, step); err != nil {
			return res, err
		}
	} else {
		res.UsedRecovery = true
	}
	if req.Via == ViaLogin {
		if _, err := tx.Exec(ctx, "DELETE FROM login_challenges WHERE id = $1", req.ChallengeID); err != nil {
			return res, err
		}
	}
	if onSuccess != nil {
		if err := onSuccess(tx); err != nil {
			return SecondFactorResult{}, err
		}
	}
	if recovery {
		// Counted after onSuccess: disable deletes every code, and the audit
		// entry must say 0 left, not 9.
		n, err := countRecoveryCodes(ctx, tx, req.UserID)
		if err != nil {
			return SecondFactorResult{}, err
		}
		res.RecoveryRemaining = n
	}
	if err := tx.Commit(ctx); err != nil {
		return SecondFactorResult{}, err
	}
	return res, nil
}

// rowQuerier is what the lock count needs, satisfied by the pool and a tx.
type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// mfaLockedTx counts the account's wrong second-factor codes in the last
// MFALockWindow and returns the lock error when there are MFALockFailures. The
// username is read from users, never from a request: the lock is keyed on
// lower(users.username), whatever casing the client typed.
func mfaLockedTx(ctx context.Context, q rowQuerier, userID int64) (*MFALockedError, error) {
	var (
		n      int
		oldest *time.Time
		now    time.Time
	)
	err := q.QueryRow(ctx, `SELECT COUNT(*), MIN(created_at), now() FROM login_attempts
		WHERE stage = $3 AND NOT success AND created_at > now() - make_interval(secs => $2)
		AND username = (SELECT lower(username) FROM users WHERE id = $1)`,
		userID, MFALockWindow.Seconds(), loginStageMFA).Scan(&n, &oldest, &now)
	if err != nil {
		return nil, err
	}
	if n < MFALockFailures || oldest == nil {
		return nil, nil
	}
	retry := oldest.Add(MFALockWindow).Sub(now)
	if retry < time.Second {
		retry = time.Second
	}
	return &MFALockedError{RetryAfter: retry}, nil
}

// MFALocked reports, without taking any lock, whether the account's second
// factor is locked right now. The first step of the login asks it after the
// password was right, so it can answer 429 instead of opening a challenge that
// could not be answered. It need not be atomic: a challenge it lets through
// still goes through the gate.
func (s *Store) MFALocked(ctx context.Context, userID int64) (*MFALockedError, error) {
	return mfaLockedTx(ctx, s.pool, userID)
}

// HasAuditSince reports whether an audit entry of the action with the target
// exists at or after since. The lock's audit entry is written once per lock
// episode with it, not once per refused request.
func (s *Store) HasAuditSince(ctx context.Context, action string, targetID int64, since time.Time) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM audit_logs
		WHERE action = $1 AND target_id = $2 AND created_at >= $3)`, action, targetID, since.UTC()).Scan(&ok)
	return ok, err
}
