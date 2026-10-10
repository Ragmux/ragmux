-- Backfill the v0.5 gateway scopes onto existing chat keys (ADR-019).
--
-- v0.5 adds POST /v1/embeddings, /v1/retrieve and /v1/rerank, guarded by the
-- scopes "embeddings", "retrieve" and "rerank". Every gateway key that can
-- already call chat completions receives the three, so an upgrade does not
-- lock a working key out of the new endpoints. This is a deliberate widening
-- of what those keys may do and is called out as a security-relevant change
-- in the changelog.
--
-- Properties:
--   * Idempotent: a scope is appended only when the key does not hold it, so a
--     second run matches no row and rewrites nothing.
--   * Additive only: no scope is removed and no existing scope is reordered.
--   * Every gateway key with "chat" is covered, revoked and expired ones
--     included, so reviving or extending one later does not change its reach.
--   * updated_at is left alone: this is a schema upgrade, not an edit by the
--     key's owner, and it must not disturb "recently changed" listings.
--   * Keys holding only "models" and all management keys are not touched.
--
-- Unknown values are validated in the application (clipScopes), not by a CHECK
-- constraint, so a new scope never needs a migration of its own.
UPDATE api_keys
   SET scopes = scopes || ARRAY(
           SELECT s FROM unnest(ARRAY['embeddings','retrieve','rerank']) WITH ORDINALITY AS u(s, n)
            WHERE NOT (s = ANY(api_keys.scopes))
            ORDER BY n)
 WHERE kind = 'gateway'
   AND 'chat' = ANY(scopes)
   AND NOT (scopes @> ARRAY['embeddings','retrieve','rerank']);
