package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// instance_settings keys.
const (
	// settingSecretKeyCanary holds canaryPlaintext sealed with the
	// credential key. Opening it proves the key this process runs with is
	// the one the database was written with.
	settingSecretKeyCanary = "secret_key_canary"
)

// canaryPlaintext is fixed and public; its only job is to be something the
// wrong key cannot produce. The value is authenticated, not secret.
const canaryPlaintext = "ragmux:secret-key-canary:v1"

// canaryAAD binds the canary ciphertext to its row, the same way a
// connection credential is bound to its connection id.
func canaryAAD() []byte { return []byte("ragmux:instance_settings:" + settingSecretKeyCanary) }

// ErrSecretKeyMismatch is returned when the stored canary does not open
// with the key this process loaded.
var ErrSecretKeyMismatch = errors.New("SECRET_KEY does not match the one this database was written with")

// verifySecretKey seals the canary on a database that has none yet and
// checks it everywhere else. It runs at Open, before anything reads a
// stored credential: a diverged key is an operator mistake (a second
// replica without SECRET_KEY, a dump restored onto a new host, a key
// rotated on one node only, a wiped data volume) and it is far cheaper to
// find at boot than on every provider call.
//
// Racing replicas are fine: the insert does nothing when a row is already
// there, so whoever gets in first decides and the others verify against it.
func (s *Store) verifySecretKey(ctx context.Context) error {
	sealed, err := s.cipher.encrypt(canaryPlaintext, canaryAAD())
	if err != nil {
		return err
	}
	// Only seal a database that has no canary once this key is known to be
	// the right one, which means asking the credentials that are already
	// there. Sealing unconditionally inverts the whole check on an upgrade:
	// a database written before this table existed has credentials but no
	// canary, so starting it once with the wrong key would mint a canary
	// saying the wrong key is right -- the boot succeeds, every provider
	// call fails, and the correct key is then refused for good, rotate-key
	// included, because that opens the store too.
	if err := s.canaryPrecondition(ctx); err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO instance_settings (key, value) VALUES ($1, $2)
		ON CONFLICT (key) DO NOTHING`, settingSecretKeyCanary, sealed); err != nil {
		return fmt.Errorf("write secret key canary: %w", err)
	}
	var stored []byte
	if err := s.pool.QueryRow(ctx, "SELECT value FROM instance_settings WHERE key = $1",
		settingSecretKeyCanary).Scan(&stored); err != nil {
		return fmt.Errorf("read secret key canary: %w", err)
	}
	plain, err := s.cipher.decrypt(stored, canaryAAD())
	if err != nil || plain != canaryPlaintext {
		return fmt.Errorf("%w (key source: %s): the canary stored in instance_settings does not decrypt. "+
			"Start with the original key, or run \"ragmux rotate-key\" with it in the environment to move the "+
			"database to a new one. See docs/scaling.md#secret-key", ErrSecretKeyMismatch, s.SecretKeySource)
	}
	return nil
}

// canaryPrecondition decides whether this key may seal a database that has
// no canary yet. A database with no stored credential cannot contradict any
// key, so the first one to arrive is adopted. Otherwise the key has to open
// one of them, and a key that cannot is reported as the mismatch it is
// rather than written down as the truth.
func (s *Store) canaryPrecondition(ctx context.Context) error {
	var exists bool
	if err := s.pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM instance_settings WHERE key = $1)",
		settingSecretKeyCanary).Scan(&exists); err != nil {
		return fmt.Errorf("read secret key canary: %w", err)
	}
	if exists {
		return nil // the stored canary is the authority; verify against it.
	}
	var id int64
	var enc []byte
	var version int16
	err := s.pool.QueryRow(ctx,
		"SELECT id, api_key_enc, key_version FROM model_connections ORDER BY id LIMIT 1").
		Scan(&id, &enc, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // nothing encrypted yet; this key becomes the database's.
	}
	if err != nil {
		return fmt.Errorf("read a stored credential to check the key: %w", err)
	}
	if _, err := s.decryptKey(id, version, enc); err != nil {
		return fmt.Errorf("%w (key source: %s): this database already holds provider credentials "+
			"this key cannot open, and it has no canary yet, so the key is not being recorded. "+
			"Start with the key those credentials were written with. See docs/scaling.md#secret-key",
			ErrSecretKeyMismatch, s.SecretKeySource)
	}
	return nil
}

// resealCanaryTx rewrites the canary with cipher inside an ongoing
// transaction. Every key rotation must do this: the next boot verifies the
// canary before it touches anything else, so a rotation that left the old
// canary behind would make the gateway refuse to start.
func resealCanaryTx(ctx context.Context, tx pgx.Tx, c *cipher) error {
	sealed, err := c.encrypt(canaryPlaintext, canaryAAD())
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO instance_settings (key, value) VALUES ($1, $2)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`,
		settingSecretKeyCanary, sealed)
	if err != nil {
		return fmt.Errorf("re-seal secret key canary: %w", err)
	}
	return nil
}
