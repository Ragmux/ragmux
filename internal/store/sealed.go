package store

import (
	"context"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
)

// sealedColumn names one column that holds a secret sealed with the instance
// key (ADR-012, ADR-038) other than model_connections.api_key_enc, which
// predates the registry and has its own legacy handling.
//
// Every column in sealedColumns is rewritten by a key rotation, in the same
// transaction as the connections and the canary: a rotation that missed a
// column would leave a secret nobody can open after the restart. A feature
// that stores a new sealed secret adds one line to the list, nothing else.
type sealedColumn struct {
	table string
	// idCol is the integer primary key the AAD is built from.
	idCol string
	// blobCol holds nonce || ciphertext; NULL or empty means "no secret".
	blobCol string
	// versionCol records the key version the row was sealed at. Empty when
	// the table has no such column: rows are then always keyVersionBound.
	versionCol string
	// aadPrefix is the associated-data prefix; the row id is appended
	// (sealedAAD), which binds a blob to its row so it cannot be copied onto
	// another one.
	aadPrefix string
}

// sealedColumns is the registry. Table and column names are interpolated into
// SQL, so they come from this fixed list and nowhere else (sealed_internal_test
// checks they are plain identifiers).
var sealedColumns = []sealedColumn{
	{table: "webhook_endpoints", idCol: "id", blobCol: "secret_enc", versionCol: "key_version", aadPrefix: webhookAADPrefix},
	{table: "webhook_endpoints", idCol: "id", blobCol: "prev_secret_enc", versionCol: "key_version", aadPrefix: webhookAADPrefix},
	{table: "users", idCol: "id", blobCol: "totp_secret_enc", versionCol: "totp_key_version", aadPrefix: totpAADPrefix},
}

// sealedAAD returns the associated data of a registered column's row.
//
// It is not called aadFor (the shape the design sketches): that name already
// belongs to the connection key-version switch in crypto.go, which this phase
// may not touch.
func sealedAAD(prefix string, id int64) []byte {
	return []byte(prefix + strconv.FormatInt(id, 10))
}

// SealedCount is the number of rows of one registered column a rotation
// rewrote.
type SealedCount struct {
	Table  string
	Column string
	Rows   int
}

// SealedReport says what ReencryptSealed rewrote.
type SealedReport struct {
	// Connections is the number of model_connections rows.
	Connections int
	// Columns has one entry per registered column, in registry order.
	Columns []SealedCount
}

// ReencryptSealed re-encrypts every secret the instance key protects -- the
// provider keys, the canary and each column in the registry -- with the key
// given as 64 hex characters, in one transaction, verifying every row opens
// with the new key before anything commits. The store keeps using its current
// key; restart the gateway with the new SECRET_KEY afterwards.
func (s *Store) ReencryptSealed(ctx context.Context, newKeyHex string) (SealedReport, error) {
	newCipher, _, err := loadCipher(newKeyHex, "", s.log)
	if err != nil {
		return SealedReport{}, err
	}
	return s.reencryptSealed(ctx, newCipher)
}

// reencryptSealed is ReencryptSealed for a cipher already built, which is what
// a key source other than a hex string will hand over.
func (s *Store) reencryptSealed(ctx context.Context, to *cipher) (SealedReport, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return SealedReport{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful commit

	var rep SealedReport
	todo, err := lockedRows(ctx, tx, "")
	if err != nil {
		return SealedReport{}, err
	}
	if err := s.resealRows(ctx, tx, to, todo); err != nil {
		return SealedReport{}, err
	}
	rep.Connections = len(todo)
	// The canary moves with the credentials, in the same transaction: the
	// next start verifies it before it reads anything, so a rotation that
	// left it sealed with the old key would refuse to boot.
	if err := resealCanaryTx(ctx, tx, to); err != nil {
		return SealedReport{}, err
	}
	for _, col := range sealedColumns {
		n, err := s.resealColumn(ctx, tx, to, col)
		if err != nil {
			return SealedReport{}, err
		}
		rep.Columns = append(rep.Columns, SealedCount{Table: col.table, Column: col.blobCol, Rows: n})
	}
	if err := tx.Commit(ctx); err != nil {
		return SealedReport{}, err
	}
	return rep, nil
}

// resealColumn rewrites every non-empty blob of col with to, locking the rows
// first, and returns how many it rewrote.
func (s *Store) resealColumn(ctx context.Context, tx pgx.Tx, to *cipher, col sealedColumn) (int, error) {
	version := "1::smallint"
	if col.versionCol != "" {
		version = col.versionCol
	}
	type sealedRow struct {
		id      int64
		version int16
		blob    []byte
	}
	rows, err := tx.Query(ctx, fmt.Sprintf(
		"SELECT %[2]s, %[3]s, %[4]s FROM %[1]s WHERE %[3]s IS NOT NULL AND octet_length(%[3]s) > 0 ORDER BY %[2]s FOR UPDATE",
		col.table, col.idCol, col.blobCol, version))
	if err != nil {
		return 0, err
	}
	var todo []sealedRow
	for rows.Next() {
		var r sealedRow
		if err := rows.Scan(&r.id, &r.blob, &r.version); err != nil {
			rows.Close()
			return 0, err
		}
		todo = append(todo, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	update := fmt.Sprintf("UPDATE %[1]s SET %[3]s = $1 WHERE %[2]s = $2", col.table, col.idCol, col.blobCol)
	if col.versionCol != "" {
		update = fmt.Sprintf("UPDATE %[1]s SET %[3]s = $1, %[4]s = %[5]d WHERE %[2]s = $2",
			col.table, col.idCol, col.blobCol, col.versionCol, keyVersionBound)
	}
	for _, r := range todo {
		// These columns never existed without associated data.
		if r.version != keyVersionBound {
			return 0, fmt.Errorf("%s.%s row %d: unsupported key version %d", col.table, col.blobCol, r.id, r.version)
		}
		aad := sealedAAD(col.aadPrefix, r.id)
		plain, err := s.cipher.decrypt(r.blob, aad)
		if err != nil {
			return 0, fmt.Errorf("decrypt %s.%s row %d: %w", col.table, col.blobCol, r.id, err)
		}
		enc, err := to.encrypt(plain, aad)
		if err != nil {
			return 0, err
		}
		if back, err := to.decrypt(enc, aad); err != nil || back != plain {
			return 0, fmt.Errorf("verify re-encrypted %s.%s row %d: %v", col.table, col.blobCol, r.id, err)
		}
		if _, err := tx.Exec(ctx, update, enc, r.id); err != nil {
			return 0, err
		}
	}
	return len(todo), nil
}
