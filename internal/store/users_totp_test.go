package store_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ragmux/ragmux/internal/auth"
	"github.com/ragmux/ragmux/internal/store"
	"github.com/ragmux/ragmux/internal/testdb"
)

// totpUser is an account with TOTP switched on and its recovery codes.
type totpUser struct {
	*store.User
	secret   string
	recovery []string // display form
}

// newTOTPUser creates the account (casing preserved), gives it a sealed secret
// through the store and switches TOTP on the way the enable endpoint does, with
// ten recovery codes.
func newTOTPUser(t *testing.T, s *store.Store, name string) *totpUser {
	t.Helper()
	ctx := context.Background()
	u, err := s.CreateUser(ctx, name, "h", "viewer")
	if err != nil {
		t.Fatal(err)
	}
	secret, err := auth.GenerateTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.SetTOTPSecret(ctx, u.ID, secret); err != nil || !ok {
		t.Fatalf("SetTOTPSecret = %v, %v", ok, err)
	}
	codes, hashes, err := auth.GenerateRecoveryCodes(auth.RecoveryCodeCount)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.DB().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := store.EnableTOTPTx(ctx, tx, u.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceRecoveryCodesTx(ctx, tx, u.ID, hashes); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return &totpUser{User: u, secret: secret, recovery: codes}
}

// code is the authenticator's code d steps from now.
func (u *totpUser) code(t *testing.T, d int) string {
	t.Helper()
	c, err := auth.TOTPCodeAt(u.secret, time.Now().Add(time.Duration(d)*auth.TOTPPeriod*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// proof builds a TOTP attempt; viaLogin attempts need a challenge id.
func (u *totpUser) proof(via, code string, challenge int64) store.SecondFactor {
	return store.SecondFactor{UserID: u.ID, Via: via, ChallengeID: challenge, IP: "10.0.0.1", Code: code, Check: auth.CheckTOTP}
}

func (u *totpUser) recoveryProof(t *testing.T, via string, i int, challenge int64) store.SecondFactor {
	t.Helper()
	norm, ok := auth.NormalizeRecoveryCode(u.recovery[i])
	if !ok {
		t.Fatalf("recovery code %q", u.recovery[i])
	}
	return store.SecondFactor{UserID: u.ID, Via: via, ChallengeID: challenge, IP: "10.0.0.1", RecoveryHash: auth.HashRecoveryCode(norm)}
}

func (u *totpUser) forgetStep(t *testing.T, s *store.Store) {
	t.Helper()
	if _, err := s.DB().Exec(context.Background(), "UPDATE users SET totp_last_step = 0 WHERE id = $1", u.ID); err != nil {
		t.Fatal(err)
	}
}

func mfaRows(t *testing.T, s *store.Store, username string) int {
	t.Helper()
	var n int
	if err := s.DB().QueryRow(context.Background(),
		"SELECT COUNT(*) FROM login_attempts WHERE stage = 'mfa' AND NOT success AND username = $1", username).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func openChallenge(t *testing.T, s *store.Store, u *totpUser, token string) *store.LoginChallenge {
	t.Helper()
	ch, err := s.CreateLoginChallenge(context.Background(), u.ID, token, false, "10.0.0.1", store.LoginChallengeTTL)
	if err != nil {
		t.Fatal(err)
	}
	return ch
}

// T5: five wrong codes lock the gate for every door, a right code included; the
// rows are keyed on lower(users.username), however the account's name is cased;
// the lock lets go when the oldest failure leaves the window.
func TestSecondFactorLockAfterFiveWrongCodes(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	u := newTOTPUser(t, s, "Ali")

	for i := 0; i < store.MFALockFailures; i++ {
		via := []string{store.ViaDisable, store.ViaRecoveryCodes}[i%2]
		if _, err := s.VerifySecondFactor(ctx, u.proof(via, "000000", 0), nil); !errors.Is(err, store.ErrMFAWrongCode) {
			t.Fatalf("wrong code %d on %s: %v", i+1, via, err)
		}
	}
	if n := mfaRows(t, s, "ali"); n != 5 {
		t.Fatalf("stage mfa rows under 'ali' = %d, want 5 (Ali must be stored lower case)", n)
	}
	if n := mfaRows(t, s, "Ali"); n != 0 {
		t.Errorf("%d rows kept the mixed-case name", n)
	}
	// Locked: the right code, a right recovery code, on any door; nothing is written.
	ch := openChallenge(t, s, u, "tok-lock")
	for name, req := range map[string]store.SecondFactor{
		"login":          u.proof(store.ViaLogin, u.code(t, 0), ch.ID),
		"disable":        u.proof(store.ViaDisable, u.code(t, 0), 0),
		"recovery-codes": u.proof(store.ViaRecoveryCodes, u.code(t, 0), 0),
		"recovery login": u.recoveryProof(t, store.ViaLogin, 0, ch.ID),
		"recovery off":   u.recoveryProof(t, store.ViaDisable, 1, 0),
	} {
		_, err := s.VerifySecondFactor(ctx, req, nil)
		var locked *store.MFALockedError
		if !errors.As(err, &locked) || locked.RetryAfter <= 0 || locked.RetryAfter > store.MFALockWindow {
			t.Errorf("%s while locked: %v, want *MFALockedError within (0, 15m]", name, err)
		}
	}
	if n := mfaRows(t, s, "ali"); n != 5 {
		t.Errorf("a refused attempt wrote a row: %d", n)
	}
	if l, err := s.MFALocked(ctx, u.ID); err != nil || l == nil {
		t.Errorf("MFALocked = %v, %v; want the lock", l, err)
	}
	// The locked login attempts did not spend the challenge.
	if _, err := s.GetLoginChallenge(ctx, "tok-lock"); err != nil {
		t.Errorf("the challenge was spent by a refused attempt: %v", err)
	}

	// The oldest failure leaves the window: one slot is free and the right code works.
	if _, err := s.DB().Exec(ctx, `UPDATE login_attempts SET created_at = now() - interval '16 minutes'
		WHERE id = (SELECT min(id) FROM login_attempts WHERE stage = 'mfa')`); err != nil {
		t.Fatal(err)
	}
	if l, err := s.MFALocked(ctx, u.ID); err != nil || l != nil {
		t.Fatalf("MFALocked after the window moved = %v, %v", l, err)
	}
	if _, err := s.VerifySecondFactor(ctx, u.proof(store.ViaDisable, u.code(t, 0), 0), nil); err != nil {
		t.Errorf("right code after the lock lifted: %v", err)
	}
}

// Failures of the password stage are another counter: five of them leave the
// second factor open.
func TestSecondFactorIgnoresPasswordFailures(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	u := newTOTPUser(t, s, "bob")
	for i := 0; i < 8; i++ {
		if err := s.RecordLoginAttempt(ctx, "bob", "10.0.0.1", false); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.VerifySecondFactor(ctx, u.proof(store.ViaDisable, u.code(t, 0), 0), nil); err != nil {
		t.Errorf("password failures locked the second factor: %v", err)
	}
	// And the other way round: second-factor rows do not reach the password counters.
	for i := 0; i < 3; i++ {
		if _, err := s.VerifySecondFactor(ctx, u.proof(store.ViaDisable, "000000", 0), nil); !errors.Is(err, store.ErrMFAWrongCode) {
			t.Fatal(err)
		}
	}
	byUser, byIP, err := s.CountFailedLoginAttempts(ctx, "BOB", "10.0.0.1", time.Now().Add(-time.Hour))
	if err != nil || byUser != 8 || byIP != 8 {
		t.Errorf("password counters = %d / %d (%v), want 8 / 8: the mfa rows leaked in", byUser, byIP, err)
	}
}

func TestSecondFactorReplayAndOrder(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	u := newTOTPUser(t, s, "carol")
	code := u.code(t, 0)
	if _, err := s.VerifySecondFactor(ctx, u.proof(store.ViaDisable, code, 0), nil); err != nil {
		t.Fatal(err)
	}
	// The same code, and the one before it, are spent; the next step is not.
	if _, err := s.VerifySecondFactor(ctx, u.proof(store.ViaDisable, code, 0), nil); !errors.Is(err, store.ErrMFAWrongCode) {
		t.Errorf("the same code twice: %v", err)
	}
	if _, err := s.VerifySecondFactor(ctx, u.proof(store.ViaDisable, u.code(t, -1), 0), nil); !errors.Is(err, store.ErrMFAWrongCode) {
		t.Errorf("an older step: %v", err)
	}
	if _, err := s.VerifySecondFactor(ctx, u.proof(store.ViaDisable, u.code(t, 1), 0), nil); err != nil {
		t.Errorf("the next step: %v", err)
	}
}

// A challenge is single use, takes five tries, and expires; the gate reads it
// again under the lock, so a stale read cannot open a second session.
func TestLoginChallengeLifecycle(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	u := newTOTPUser(t, s, "dave")

	ch := openChallenge(t, s, u, "tok-1")
	if ch.ExpiresAt.Before(time.Now().Add(4*time.Minute)) || ch.ExpiresAt.After(time.Now().Add(6*time.Minute)) {
		t.Errorf("expires_at = %v, want about five minutes ahead", ch.ExpiresAt)
	}
	var stored string
	if err := s.DB().QueryRow(ctx, "SELECT token_hash FROM login_challenges WHERE id = $1", ch.ID).Scan(&stored); err != nil || stored == "tok-1" || stored != store.HashToken("tok-1") {
		t.Errorf("token stored as %q (%v), want its hash", stored, err)
	}
	got, err := s.GetLoginChallenge(ctx, "tok-1")
	if err != nil || got.ID != ch.ID || got.UserID != u.ID {
		t.Fatalf("GetLoginChallenge = %+v, %v", got, err)
	}

	// Spent by the first success: the second proof, a different valid one, finds
	// nothing, although its caller looked the challenge up before.
	stale := u.proof(store.ViaLogin, u.code(t, 0), ch.ID)
	other := u.recoveryProof(t, store.ViaLogin, 0, ch.ID)
	if _, err := s.VerifySecondFactor(ctx, stale, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.VerifySecondFactor(ctx, other, nil); !errors.Is(err, store.ErrMFAInvalidChallenge) {
		t.Errorf("second proof on a spent challenge: %v", err)
	}
	if _, err := s.GetLoginChallenge(ctx, "tok-1"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a spent challenge is still found: %v", err)
	}
	if n, err := unusedRecoveryCodes(t, s, u.ID); err != nil || n != 10 {
		t.Errorf("the refused recovery code was spent anyway: %d, %v", n, err)
	}

	// Expired.
	u.forgetStep(t, s)
	ch = openChallenge(t, s, u, "tok-2")
	if _, err := s.DB().Exec(ctx, "UPDATE login_challenges SET expires_at = now() - interval '1 second' WHERE id = $1", ch.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetLoginChallenge(ctx, "tok-2"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("an expired challenge was found: %v", err)
	}
	if _, err := s.VerifySecondFactor(ctx, u.proof(store.ViaLogin, u.code(t, 0), ch.ID), nil); !errors.Is(err, store.ErrMFAInvalidChallenge) {
		t.Errorf("an expired challenge was honoured: %v", err)
	}

	// The fifth wrong code deletes it.
	ch = openChallenge(t, s, u, "tok-3")
	for i := 1; i <= store.LoginChallengeMaxAttempts; i++ {
		res, err := s.VerifySecondFactor(ctx, u.proof(store.ViaLogin, "000000", ch.ID), nil)
		if !errors.Is(err, store.ErrMFAWrongCode) || res.AttemptsLeft != store.LoginChallengeMaxAttempts-i {
			t.Fatalf("wrong code %d: %v, attempts left %d", i, err, res.AttemptsLeft)
		}
		_, gerr := s.GetLoginChallenge(ctx, "tok-3")
		if closed := errors.Is(gerr, store.ErrNotFound); closed != (i == store.LoginChallengeMaxAttempts) {
			t.Fatalf("after wrong code %d the challenge closed = %v", i, closed)
		}
	}
}

func TestRecoveryCodeSingleUse(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	u := newTOTPUser(t, s, "erin")

	var hash string
	if err := s.DB().QueryRow(ctx, "SELECT code_hash FROM user_recovery_codes WHERE user_id = $1 LIMIT 1", u.ID).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	for _, c := range u.recovery {
		if strings.Contains(hash, strings.ReplaceAll(c, "-", "")) || len(hash) != 64 {
			t.Fatalf("the stored value %q looks like a code, not a SHA-256", hash)
		}
	}
	res, err := s.VerifySecondFactor(ctx, u.recoveryProof(t, store.ViaDisable, 3, 0), nil)
	if err != nil || !res.UsedRecovery || res.RecoveryRemaining != 9 {
		t.Fatalf("first use: %+v, %v", res, err)
	}
	if _, err := s.VerifySecondFactor(ctx, u.recoveryProof(t, store.ViaDisable, 3, 0), nil); !errors.Is(err, store.ErrMFAWrongCode) {
		t.Errorf("second use of the same recovery code: %v", err)
	}
	if n, _ := unusedRecoveryCodes(t, s, u.ID); n != 9 {
		t.Errorf("unused codes = %d, want 9", n)
	}
}

// T5 concurrent: whatever the number of parallel requests, the rows and the
// successes follow the rule, not the timing.
func TestSecondFactorConcurrent(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)

	run := func(n int, f func(i int) error) []error {
		errs := make([]error, n)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				errs[i] = f(i)
			}(i)
		}
		close(start)
		wg.Wait()
		return errs
	}
	count := func(errs []error, target error) (n int) {
		for _, e := range errs {
			if e == nil && target == nil || e != nil && target != nil && errors.Is(e, target) {
				n++
			}
		}
		return n
	}

	t.Run("wrong codes", func(t *testing.T) {
		u := newTOTPUser(t, s, "Frank")
		errs := run(16, func(int) error {
			_, err := s.VerifySecondFactor(ctx, u.proof(store.ViaDisable, "000000", 0), nil)
			return err
		})
		if n := mfaRows(t, s, "frank"); n > store.MFALockFailures || n < 1 {
			t.Errorf("%d mfa rows from 16 parallel wrong codes, want 1..5", n)
		}
		for _, err := range errs {
			var locked *store.MFALockedError
			if !errors.Is(err, store.ErrMFAWrongCode) && !errors.Is(err, store.ErrMFABusy) && !errors.As(err, &locked) {
				t.Errorf("unexpected outcome %v", err)
			}
		}
		if wrong := count(errs, store.ErrMFAWrongCode); wrong != mfaRows(t, s, "frank") {
			t.Errorf("%d wrong-code answers for %d rows", wrong, mfaRows(t, s, "frank"))
		}
		// Sequentially the rest of the five is reached, never exceeded.
		for i := 0; i < 8; i++ {
			_, _ = s.VerifySecondFactor(ctx, u.proof(store.ViaDisable, "000000", 0), nil)
		}
		if n := mfaRows(t, s, "frank"); n != store.MFALockFailures {
			t.Errorf("rows = %d, want exactly %d", n, store.MFALockFailures)
		}
	})

	t.Run("one TOTP code", func(t *testing.T) {
		u := newTOTPUser(t, s, "grace")
		code := u.code(t, 0)
		errs := run(8, func(int) error {
			_, err := s.VerifySecondFactor(ctx, u.proof(store.ViaDisable, code, 0), nil)
			return err
		})
		if n := count(errs, nil); n != 1 {
			t.Errorf("%d successes for one code, want 1 (%v)", n, errs)
		}
	})

	t.Run("one recovery code", func(t *testing.T) {
		u := newTOTPUser(t, s, "heidi")
		errs := run(8, func(int) error {
			_, err := s.VerifySecondFactor(ctx, u.recoveryProof(t, store.ViaDisable, 0, 0), nil)
			return err
		})
		if n := count(errs, nil); n != 1 {
			t.Errorf("%d successes for one recovery code, want 1 (%v)", n, errs)
		}
	})

	t.Run("one challenge, two valid proofs", func(t *testing.T) {
		u := newTOTPUser(t, s, "ivan")
		ch := openChallenge(t, s, u, "tok-race")
		proofs := []store.SecondFactor{
			u.proof(store.ViaLogin, u.code(t, 0), ch.ID),
			u.recoveryProof(t, store.ViaLogin, 0, ch.ID),
			u.recoveryProof(t, store.ViaLogin, 1, ch.ID),
			u.proof(store.ViaLogin, u.code(t, 1), ch.ID),
		}
		errs := run(len(proofs), func(i int) error {
			_, err := s.VerifySecondFactor(ctx, proofs[i], nil)
			return err
		})
		if n := count(errs, nil); n != 1 {
			t.Errorf("%d sessions out of one challenge, want 1 (%v)", n, errs)
		}
	})
}

// A failing onSuccess takes the code with it: nothing is spent.
func TestSecondFactorRollsBackWithOnSuccess(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	u := newTOTPUser(t, s, "judy")
	boom := errors.New("boom")
	code := u.code(t, 0)
	if _, err := s.VerifySecondFactor(ctx, u.proof(store.ViaDisable, code, 0), func(pgx.Tx) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if _, err := s.VerifySecondFactor(ctx, u.recoveryProof(t, store.ViaDisable, 0, 0), func(pgx.Tx) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if n, _ := unusedRecoveryCodes(t, s, u.ID); n != 10 {
		t.Errorf("a rolled back recovery code stayed spent: %d left", n)
	}
	if _, err := s.VerifySecondFactor(ctx, u.proof(store.ViaDisable, code, 0), nil); err != nil {
		t.Errorf("the code of a rolled back attempt is spent: %v", err)
	}
}

func TestSecondFactorStateChecks(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	// No secret at all.
	bare, err := s.CreateUser(ctx, "kim", "h", "viewer")
	if err != nil {
		t.Fatal(err)
	}
	probe := func(u *store.User, via string) error {
		_, err := s.VerifySecondFactor(ctx, store.SecondFactor{UserID: u.ID, Via: via, Code: "000000", Check: auth.CheckTOTP,
			ChallengeID: map[bool]int64{true: 1}[via == store.ViaLogin]}, nil)
		return err
	}
	if err := probe(bare, store.ViaEnable); !errors.Is(err, store.ErrTOTPNotSetUp) {
		t.Errorf("enable without setup: %v", err)
	}
	if err := probe(bare, store.ViaDisable); !errors.Is(err, store.ErrTOTPNotEnabled) {
		t.Errorf("disable without totp: %v", err)
	}
	if err := probe(bare, store.ViaRecoveryCodes); !errors.Is(err, store.ErrTOTPNotEnabled) {
		t.Errorf("recovery-codes without totp: %v", err)
	}
	if err := probe(bare, store.ViaLogin); !errors.Is(err, store.ErrMFAInvalidChallenge) {
		t.Errorf("login without totp: %v", err)
	}
	// A pending secret: enable counts a wrong code against the shared counter.
	if ok, err := s.SetTOTPSecret(ctx, bare.ID, "JBSWY3DPEHPK3PXP"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err := probe(bare, store.ViaEnable); !errors.Is(err, store.ErrMFAWrongCode) {
		t.Errorf("enable with a wrong code: %v", err)
	}
	if n := mfaRows(t, s, "kim"); n != 1 {
		t.Errorf("a wrong enable code wrote %d rows, want 1", n)
	}
	if st, _ := s.GetTOTPStatus(ctx, bare.ID); !st.HasSecret || st.Enabled {
		t.Errorf("status = %+v, want a pending secret", st)
	}
	// Enabled: enable is a 409, and SetTOTPSecret leaves the secret alone.
	on := newTOTPUser(t, s, "lee")
	if err := probe(on.User, store.ViaEnable); !errors.Is(err, store.ErrTOTPAlreadyEnabled) {
		t.Errorf("enable when enabled: %v", err)
	}
	if ok, err := s.SetTOTPSecret(ctx, on.ID, "KRSXG5CTMVRXEZLU"); err != nil || ok {
		t.Errorf("SetTOTPSecret on an enabled account = %v, %v; want false", ok, err)
	}
	if _, err := s.VerifySecondFactor(ctx, on.proof(store.ViaDisable, on.code(t, 0), 0), nil); err != nil {
		t.Errorf("the original secret stopped working: %v", err)
	}
	// Malformed attempts are programming errors, not guesses.
	for name, req := range map[string]store.SecondFactor{
		"no proof":       {UserID: on.ID, Via: store.ViaDisable},
		"two proofs":     {UserID: on.ID, Via: store.ViaDisable, Code: "1", Check: auth.CheckTOTP, RecoveryHash: "x"},
		"unknown via":    {UserID: on.ID, Via: "x", Code: "1", Check: auth.CheckTOTP},
		"enable+codes":   {UserID: on.ID, Via: store.ViaEnable, RecoveryHash: "x"},
		"login no chall": {UserID: on.ID, Via: store.ViaLogin, Code: "1", Check: auth.CheckTOTP},
	} {
		if _, err := s.VerifySecondFactor(ctx, req, nil); err == nil || errors.Is(err, store.ErrMFAWrongCode) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if n := mfaRows(t, s, "lee"); n != 0 {
		t.Errorf("malformed attempts wrote %d rows", n)
	}
}

// The secret is stored sealed, bound to its row, and survives rotate-key.
func TestTOTPSecretSealedAndRotated(t *testing.T) {
	ctx := context.Background()
	cfg := testdb.Config(t)
	s := testdb.OpenWith(t, cfg)
	u := newTOTPUser(t, s, "mallory")
	other := newTOTPUser(t, s, "nina")

	var raw []byte
	var ver int16
	if err := s.DB().QueryRow(ctx, "SELECT totp_secret_enc, totp_key_version FROM users WHERE id = $1", u.ID).Scan(&raw, &ver); err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 || ver != 1 || strings.Contains(string(raw), u.secret) {
		t.Fatalf("stored secret: %d bytes, version %d, plaintext inside: %v", len(raw), ver, strings.Contains(string(raw), u.secret))
	}

	rep, err := s.ReencryptSealed(ctx, rotatedKey)
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]int{}
	for _, c := range rep.Columns {
		rows[c.Table+"."+c.Column] = c.Rows
	}
	if rows["users.totp_secret_enc"] != 2 {
		t.Errorf("columns = %v, want users.totp_secret_enc 2", rows)
	}
	cfg.SecretKeyHex = rotatedKey
	s2 := testdb.OpenWith(t, cfg)
	if _, err := s2.VerifySecondFactor(ctx, u.proof(store.ViaDisable, u.code(t, 0), 0), nil); err != nil {
		t.Errorf("TOTP after rotate-key: %v", err)
	}
	// The old key no longer opens it (the store still holds the old cipher).
	if _, err := s.VerifySecondFactor(ctx, other.proof(store.ViaDisable, other.code(t, 0), 0), nil); err == nil || errors.Is(err, store.ErrMFAWrongCode) {
		t.Errorf("the old key opened a rotated secret: %v", err)
	}
	// Bound to the row: one account's blob copied onto another does not open.
	if _, err := s2.DB().Exec(ctx, "UPDATE users SET totp_secret_enc = (SELECT totp_secret_enc FROM users WHERE id = $1) WHERE id = $2", u.ID, other.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.VerifySecondFactor(ctx, other.proof(store.ViaDisable, u.code(t, 1), 0), nil); err == nil || errors.Is(err, store.ErrMFAWrongCode) {
		t.Errorf("a secret copied onto another row opened: %v", err)
	}
}

// The exits: an admin reset, the CLI and a password reset forget the second
// factor's failures and challenges, under the same spelling the gate writes
// them with; a change of one's own password closes the challenges only.
func TestMFAExitsClearByLowerCasedName(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	u := newTOTPUser(t, s, "Olga")
	bystander := newTOTPUser(t, s, "peggy")
	for _, x := range []*totpUser{u, bystander} {
		for i := 0; i < 3; i++ {
			if _, err := s.VerifySecondFactor(ctx, x.proof(store.ViaDisable, "000000", 0), nil); !errors.Is(err, store.ErrMFAWrongCode) {
				t.Fatal(err)
			}
		}
	}
	openChallenge(t, s, u, "tok-o")
	openChallenge(t, s, bystander, "tok-p")

	if err := s.DeleteUserLoginChallenges(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetLoginChallenge(ctx, "tok-o"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the challenge survived DeleteUserLoginChallenges: %v", err)
	}
	if n := mfaRows(t, s, "olga"); n != 3 {
		t.Errorf("a password change deleted the counter: %d rows left, want 3", n)
	}

	openChallenge(t, s, u, "tok-o2")
	if err := s.ClearUserMFAState(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if n := mfaRows(t, s, "olga"); n != 0 {
		t.Errorf("ClearUserMFAState left %d rows", n)
	}
	if _, err := s.GetLoginChallenge(ctx, "tok-o2"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the challenge survived ClearUserMFAState: %v", err)
	}
	if n := mfaRows(t, s, "peggy"); n != 3 {
		t.Errorf("another account's rows were touched: %d", n)
	}
	if _, err := s.GetLoginChallenge(ctx, "tok-p"); err != nil {
		t.Errorf("another account's challenge was touched: %v", err)
	}
	if st, _ := s.GetTOTPStatus(ctx, u.ID); !st.Enabled {
		t.Error("ClearUserMFAState switched TOTP off")
	}

	// DisableUserTOTP: the secret, the codes, the rows and the challenges go.
	for i := 0; i < 2; i++ {
		_, _ = s.VerifySecondFactor(ctx, u.proof(store.ViaDisable, "000000", 0), nil)
	}
	openChallenge(t, s, u, "tok-o3")
	had, err := s.DisableUserTOTP(ctx, u.ID)
	if err != nil || !had {
		t.Fatalf("DisableUserTOTP = %v, %v", had, err)
	}
	if st, _ := s.GetTOTPStatus(ctx, u.ID); st.Enabled || st.HasSecret {
		t.Errorf("status after disable = %+v", st)
	}
	if n, _ := unusedRecoveryCodes(t, s, u.ID); n != 0 {
		t.Errorf("%d recovery codes survived", n)
	}
	if n := mfaRows(t, s, "olga"); n != 0 {
		t.Errorf("%d rows survived", n)
	}
	if had, err := s.DisableUserTOTP(ctx, u.ID); err != nil || had {
		t.Errorf("a second DisableUserTOTP = %v, %v; want false, nil", had, err)
	}
}

// T10: the count comes before the password is tried, so the budget cannot be
// outrun in parallel.
func TestSessionPasswordBudget(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	u, err := s.CreateUser(ctx, "quinn", "h", "viewer")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession(ctx, u.ID, "tok-q", time.Hour); err != nil {
		t.Fatal(err)
	}
	h := store.HashToken("tok-q")
	for i := 1; i <= store.SessionPasswordBudget; i++ {
		if n, ok, err := s.ClaimSessionPasswordAttempt(ctx, h); err != nil || !ok || n != i {
			t.Fatalf("claim %d = %d, %v, %v", i, n, ok, err)
		}
	}
	if _, ok, err := s.ClaimSessionPasswordAttempt(ctx, h); err != nil || ok {
		t.Errorf("the sixth claim: ok=%v err=%v, want refused", ok, err)
	}
	if err := s.ResetSessionPasswordAttempts(ctx, h); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	granted := 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok, err := s.ClaimSessionPasswordAttempt(ctx, h); err == nil && ok {
				mu.Lock()
				granted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if granted != store.SessionPasswordBudget {
		t.Errorf("20 parallel claims granted %d, want %d", granted, store.SessionPasswordBudget)
	}
	if _, ok, _ := s.ClaimSessionPasswordAttempt(ctx, store.HashToken("no-such-session")); ok {
		t.Error("a session that does not exist was granted a try")
	}
}

func TestRetainLoginChallenges(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	u := newTOTPUser(t, s, "ruth")
	openChallenge(t, s, u, "tok-old")
	openChallenge(t, s, u, "tok-new")
	if _, err := s.DB().Exec(ctx, "UPDATE login_challenges SET expires_at = now() - interval '1 minute' WHERE token_hash = $1", store.HashToken("tok-old")); err != nil {
		t.Fatal(err)
	}
	n, err := s.DeleteExpiredBatch(ctx, store.RetainLoginChallenges, time.Now(), 10)
	if err != nil || n != 1 {
		t.Fatalf("DeleteExpiredBatch = %d, %v; want 1", n, err)
	}
	if _, err := s.GetLoginChallenge(ctx, "tok-new"); err != nil {
		t.Errorf("a live challenge was deleted: %v", err)
	}
}

// While another check holds the account's row, an attempt is refused at once
// instead of queueing for a pooled connection, and it counts for nothing: the
// code was not looked at.
func TestSecondFactorBusyRefusesWithoutWaiting(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	u := newTOTPUser(t, s, "sam")
	holder, err := s.DB().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx, "SELECT 1 FROM users WHERE id = $1 FOR NO KEY UPDATE", u.ID); err != nil {
		t.Fatal(err)
	}
	begun := time.Now()
	for _, code := range []string{"000000", u.code(t, 0)} {
		if _, err := s.VerifySecondFactor(ctx, u.proof(store.ViaDisable, code, 0), nil); !errors.Is(err, store.ErrMFABusy) {
			t.Fatalf("while the row is held: %v, want ErrMFABusy", err)
		}
	}
	if time.Since(begun) > 2*time.Second {
		t.Errorf("the refusal took %s; it must not wait for the lock", time.Since(begun))
	}
	if n := mfaRows(t, s, "sam"); n != 0 {
		t.Errorf("a busy refusal counted as an attempt: %d rows", n)
	}
	_ = holder.Rollback(ctx)
	if _, err := s.VerifySecondFactor(ctx, u.proof(store.ViaDisable, u.code(t, 0), 0), nil); err != nil {
		t.Errorf("the right code once the row is free: %v", err)
	}
}

// unusedRecoveryCodes counts a user's recovery codes that are not spent.
func unusedRecoveryCodes(t *testing.T, s *store.Store, userID int64) (int, error) {
	t.Helper()
	var n int
	err := s.DB().QueryRow(context.Background(), "SELECT COUNT(*) FROM user_recovery_codes WHERE user_id = $1 AND used_at IS NULL", userID).Scan(&n)
	return n, err
}

// A one-step login whose status read went stale must not leave a session behind
// an enable: the insert waits for an enable in flight and then refuses.
func TestCreateSessionIfNoTOTPWaitsForAnEnableInFlight(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	u, err := s.CreateUser(ctx, "tess", "h", "viewer")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.CreateSessionIfNoTOTP(ctx, u.ID, "tok-plain", time.Hour); err != nil || !ok {
		t.Fatalf("without TOTP: %v, %v", ok, err)
	}
	if ok, err := s.SetTOTPSecret(ctx, u.ID, "JBSWY3DPEHPK3PXP"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	// The enable, as the endpoint runs it: the row is updated and the other
	// sessions are deleted in one transaction that has not committed yet.
	enable, err := s.DB().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = enable.Rollback(ctx) }()
	if err := store.EnableTOTPTx(ctx, enable, u.ID); err != nil {
		t.Fatal(err)
	}
	type result struct {
		ok  bool
		err error
	}
	done := make(chan result, 1)
	go func() {
		ok, err := s.CreateSessionIfNoTOTP(ctx, u.ID, "tok-racer", time.Hour)
		done <- result{ok, err}
	}()
	select {
	case r := <-done:
		t.Fatalf("the insert did not wait for the enable in flight: %+v", r)
	case <-time.After(300 * time.Millisecond):
	}
	if err := store.DeleteUserSessionsExceptTx(ctx, enable, u.ID, "none"); err != nil {
		t.Fatal(err)
	}
	if err := enable.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if r := <-done; r.err != nil || r.ok {
		t.Errorf("after the enable: %+v, want a refusal", r)
	}
	var n int
	if err := s.DB().QueryRow(ctx, "SELECT COUNT(*) FROM sessions WHERE user_id = $1", u.ID).Scan(&n); err != nil || n != 0 {
		t.Errorf("%d sessions of the account survived the enable (%v)", n, err)
	}
}

// The admin summary describes the password limiter: second-factor failures are
// not in it.
func TestFailedLoginSummaryIgnoresMFAStage(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	u := newTOTPUser(t, s, "uma")
	for i := 0; i < 3; i++ {
		if _, err := s.VerifySecondFactor(ctx, u.proof(store.ViaDisable, "000000", 0), nil); !errors.Is(err, store.ErrMFAWrongCode) {
			t.Fatal(err)
		}
	}
	if err := s.RecordLoginAttempt(ctx, "uma", "10.0.0.1", false); err != nil {
		t.Fatal(err)
	}
	if n, err := s.CountFailedLoginAttemptsSince(ctx, time.Now().Add(-time.Hour)); err != nil || n != 1 {
		t.Errorf("failed logins = %d, %v; want the 1 password failure", n, err)
	}
}

// Disable deletes every recovery code, so a recovery code spent on it leaves
// none, and the result must say so.
func TestRecoveryRemainingCountsAfterOnSuccess(t *testing.T) {
	ctx := context.Background()
	s := testdb.Open(t)
	u := newTOTPUser(t, s, "vic")
	res, err := s.VerifySecondFactor(ctx, u.recoveryProof(t, store.ViaDisable, 0, 0), func(tx pgx.Tx) error {
		return store.ClearTOTPTx(ctx, tx, u.ID)
	})
	if err != nil || !res.UsedRecovery || res.RecoveryRemaining != 0 {
		t.Errorf("disable with a recovery code: %+v, %v; want 0 remaining", res, err)
	}
}
