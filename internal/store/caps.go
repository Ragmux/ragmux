package store

import (
	"context"
	"strconv"
	"time"
)

// Advisory lock keys for work that must run on one replica at a time.
// Unlike the transaction-scoped migration lock these are session locks:
// they live on a connection and are released when it goes away, which is
// what makes them safe for a leader that may be killed at any moment.
const (
	// LockJanitor serialises the retention pass across replicas.
	LockJanitor int64 = 0x7261676d75780002 // "ragmux" + 2
)

// TryAdvisoryLock takes the session-level advisory lock for key without
// waiting. It reports whether the lock was acquired and returns the
// function that releases it; release is nil when ok is false.
//
// The lock pins the connection it was taken on: a session advisory lock
// belongs to its connection, so the pool must not hand that connection to
// anybody else before the unlock. release returns it.
//
// The key is combined with the current schema, so two Ragmux instances
// sharing one database in separate schemas elect their own leader (and so
// tests, which are isolated by schema, do not fight over one lock).
func (s *Store) TryAdvisoryLock(ctx context.Context, key int64) (func(), bool, error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	var scoped int64
	if err := conn.QueryRow(ctx, "SELECT hashtext(current_schema() || ':' || $1)::bigint",
		strconv.FormatInt(key, 10)).Scan(&scoped); err != nil {
		conn.Release()
		return nil, false, err
	}
	var ok bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", scoped).Scan(&ok); err != nil {
		conn.Release()
		return nil, false, err
	}
	if !ok {
		conn.Release()
		return nil, false, nil
	}
	return func() {
		// The caller's context is usually already cancelled by the time a
		// leader shuts down, so the unlock gets its own budget. Dropping
		// the connection would release the lock anyway.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", scoped); err != nil {
			s.log.Warn("release advisory lock", "key", key, "err", err)
		}
		conn.Release()
	}, true, nil
}
