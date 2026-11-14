package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"

	"github.com/jackc/pgx/v5"
)

// KVKK anonymisation (ADR-040). A pseudonym is generated once per operation
// and written to every row the operation touches; it is random, not derived
// from the original value, so nothing but the operation's own memory could
// ever map it back (and that memory is never kept).

// ErrAlreadyAnonymized is returned when the account was anonymised before.
var ErrAlreadyAnonymized = errors.New("account is already anonymized")

// ErrConfirmMismatch is returned when the confirmation text is not the
// account's current username.
var ErrConfirmMismatch = errors.New("confirmation does not match the username")

// newPseudonym returns "anon-" and 16 random hex digits.
func newPseudonym() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "anon-" + hex.EncodeToString(b[:]), nil
}

// AnonymizeUserResult reports what AnonymizeUser did. It carries the pseudonym
// and counts only, never the original username.
type AnonymizeUserResult struct {
	Pseudonym   string `json:"pseudonym"`
	RevokedKeys int64  `json:"revoked_keys"`
}

// AnonymizeUser makes an account's personal fields irreversible in one
// transaction and keeps its usage history: request_logs rows are not touched
// (they point at the account by id and carry no name).
//
// confirm must equal the account's current username. unusableHash is a bcrypt
// hash of a random secret nobody knows; the caller makes it because bcrypt
// stays outside the store. Anonymising the last active admin fails with
// ErrLastAdmin, an account that already was anonymised with
// ErrAlreadyAnonymized, a wrong confirmation with ErrConfirmMismatch.
//
// What changes, besides the account row:
//   - sessions, second-factor state and project memberships are deleted;
//   - every key of the account is revoked and renamed key-<id>;
//   - audit_logs: the actor name of the account's own entries, details.username
//     of the entries about it, the failed/limited/locked login entries that
//     carry no actor (matched on the name, case-insensitively, with no lower
//     bound in time) and details.name of the key entries (see below);
//   - login_attempts rows of the name are deleted.
//
// audit_logs.ip stays (a security record kept for AUDIT_RETENTION_DAYS).
func (s *Store) AnonymizeUser(ctx context.Context, id int64, confirm, unusableHash string) (*AnonymizeUserResult, error) {
	pseudonym, err := newPseudonym()
	if err != nil {
		return nil, err
	}
	res := &AnonymizeUserResult{Pseudonym: pseudonym}
	err = s.adminGuardedTx(ctx, id, func(tx pgx.Tx) error {
		var (
			old        string
			anonymised bool
		)
		if err := tx.QueryRow(ctx,
			"SELECT username, anonymized_at IS NOT NULL FROM users WHERE id = $1 FOR UPDATE", id).Scan(&old, &anonymised); err != nil {
			return scanErr(err)
		}
		if anonymised {
			return ErrAlreadyAnonymized
		}
		if confirm != old {
			return ErrConfirmMismatch
		}

		// The key rows first: the audit rewrite below reads api_keys to find
		// the keys that were the account's.
		if err := tx.QueryRow(ctx,
			"SELECT COUNT(*) FROM api_keys WHERE user_id = $1 AND revoked_at IS NULL", id).Scan(&res.RevokedKeys); err != nil {
			return err
		}
		// A free-text key name can carry a person's name, so it is replaced by
		// key-<id>. (user_id, name) is unique and a name such as "key-5" may
		// already be taken by another key of the account, so the rename goes
		// through names that cannot collide (the random pseudonym) first.
		if _, err := tx.Exec(ctx, `UPDATE api_keys
			SET name = $2::text || '-' || id, revoked_at = COALESCE(revoked_at, now()), updated_at = now()
			WHERE user_id = $1`, id, pseudonym); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "UPDATE api_keys SET name = 'key-' || id WHERE user_id = $1", id); err != nil {
			return err
		}

		// Audit. details.username / details.name are replaced only where the
		// field already is a string, so an entry keeps its shape.
		if _, err := tx.Exec(ctx, "UPDATE audit_logs SET actor_username = $2 WHERE actor_user_id = $1", id, pseudonym); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE audit_logs SET details = jsonb_set(details, '{username}', to_jsonb($2::text))
			WHERE target_type = 'user' AND target_id = $1 AND jsonb_typeof(details->'username') = 'string'`,
			id, pseudonym); err != nil {
			return err
		}
		// Entries with no actor: a login that named this account but never got
		// as far as an identity. The name is stored as typed, so the match is
		// case-insensitive. No lower bound: a name can be re-registered after a
		// deletion and the older rows would otherwise keep it (D6).
		if _, err := tx.Exec(ctx, `UPDATE audit_logs SET details = jsonb_set(details, '{username}', to_jsonb($1::text))
			WHERE target_type = 'user' AND target_id IS NULL
			  AND action IN ('login.failure', 'login.rate_limited', 'login.locked')
			  AND jsonb_typeof(details->'username') = 'string'
			  AND lower(details->>'username') = lower($2::text)`,
			pseudonym, old); err != nil {
			return err
		}
		// Key entries. The keys that were the account's: its api_keys rows,
		// plus any key an entry names as belonging to it (details.user_id) --
		// the second source keeps the key's other entries reachable after the
		// key row is gone. Separately, an entry the account itself made
		// (actor_user_id) is covered whoever owned the key; the key id stays
		// in target_id, so the trail is not lost.
		if _, err := tx.Exec(ctx, `WITH owned AS (
				SELECT id AS key_id FROM api_keys WHERE user_id = $1
				UNION
				SELECT target_id FROM audit_logs
				WHERE target_type = 'api_key' AND target_id IS NOT NULL AND details->>'user_id' = $1::bigint::text
			)
			UPDATE audit_logs SET details = jsonb_set(details, '{name}', to_jsonb('key-' || target_id::text))
			WHERE target_type = 'api_key' AND target_id IS NOT NULL
			  AND jsonb_typeof(details->'name') = 'string'
			  AND (target_id IN (SELECT key_id FROM owned) OR actor_user_id = $1)`, id); err != nil {
			return err
		}

		if _, err := tx.Exec(ctx, `DELETE FROM login_attempts WHERE lower(username) = lower($1::text)`, old); err != nil {
			return err
		}
		for _, q := range []string{
			"DELETE FROM sessions WHERE user_id = $1",
			"DELETE FROM user_recovery_codes WHERE user_id = $1",
			"DELETE FROM login_challenges WHERE user_id = $1",
			"DELETE FROM project_members WHERE user_id = $1",
		} {
			if _, err := tx.Exec(ctx, q, id); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `UPDATE users SET username = $2, password_hash = $3, is_active = false,
			anonymized_at = now(), totp_secret_enc = NULL, totp_enabled_at = NULL, totp_last_step = 0
			WHERE id = $1`, id, pseudonym, unusableHash)
		return err
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// endUserBatch is how many request_logs rows one UPDATE rewrites.
const endUserBatch = 5000

// AnonymizeEndUserResult reports the rows AnonymizeEndUser rewrote.
type AnonymizeEndUserResult struct {
	Pseudonym         string `json:"pseudonym"`
	RequestLogs       int64  `json:"request_logs"`
	EndUserUsage      int64  `json:"end_user_usage"`
	AlertEvents       int64  `json:"alert_events"`
	WebhookDeliveries int64  `json:"webhook_deliveries"`
}

// AnonymizeEndUser replaces an end user's identifier with one pseudonym in
// request_logs, end_user_usage, alert_events and the subject of queued or
// delivered alert webhooks. projectID limits it to one project; nil means every
// project. The usage figures and request counts do not change.
//
// request_logs is rewritten in batches of endUserBatch rows, each its own
// statement, so one call never holds a lock on a long run of rows; the small
// tables follow in one transaction. A failure partway leaves the rows already
// rewritten under the pseudonym and the rest under the original value: call
// again, and the rest gets a second pseudonym.
func (s *Store) AnonymizeEndUser(ctx context.Context, endUser string, projectID *int64) (*AnonymizeEndUserResult, error) {
	var ids []int64
	if projectID != nil {
		var ok bool
		if err := s.pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM projects WHERE id = $1)", *projectID).Scan(&ok); err != nil {
			return nil, err
		}
		if !ok {
			return nil, ErrNotFound
		}
		ids = []int64{*projectID}
	} else {
		rows, err := s.pool.Query(ctx, "SELECT id FROM projects ORDER BY id")
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return nil, err
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		rows.Close()
	}
	pseudonym, err := newPseudonym()
	if err != nil {
		return nil, err
	}
	res := &AnonymizeEndUserResult{Pseudonym: pseudonym}

	// One project at a time: idx_request_logs_end_user leads with project_id,
	// so each batch is an index range scan instead of a pass over the table.
	for _, pid := range ids {
		for {
			tag, err := s.pool.Exec(ctx, `UPDATE request_logs SET end_user = $3
				WHERE id IN (SELECT id FROM request_logs WHERE project_id = $1 AND end_user = $2 LIMIT $4)`,
				pid, endUser, pseudonym, endUserBatch)
			if err != nil {
				return nil, err
			}
			res.RequestLogs += tag.RowsAffected()
			if tag.RowsAffected() < endUserBatch {
				break
			}
		}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful commit
	tag, err := tx.Exec(ctx, "UPDATE end_user_usage SET end_user = $3 WHERE project_id = ANY($1) AND end_user = $2",
		ids, endUser, pseudonym)
	if err != nil {
		return nil, err
	}
	res.EndUserUsage = tag.RowsAffected()
	tag, err = tx.Exec(ctx, `UPDATE alert_events SET subject_id = $3
		WHERE project_id = ANY($1) AND subject_kind = 'end_user' AND subject_id = $2`, ids, endUser, pseudonym)
	if err != nil {
		return nil, err
	}
	res.AlertEvents = tag.RowsAffected()
	tag, err = tx.Exec(ctx, `UPDATE webhook_deliveries d
		SET payload = jsonb_set(d.payload, '{subject,id}', to_jsonb($3::text))
		FROM webhook_endpoints e
		WHERE e.id = d.endpoint_id AND e.project_id = ANY($1)
		  AND d.payload #>> '{subject,kind}' = 'end_user' AND d.payload #>> '{subject,id}' = $2`,
		ids, endUser, pseudonym)
	if err != nil {
		return nil, err
	}
	res.WebhookDeliveries = tag.RowsAffected()
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return res, nil
}
