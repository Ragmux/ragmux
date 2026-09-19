-- Retire the built-in custom_openai '*' price row.
--
-- The shipped table used to price every custom_openai model at 0. But
-- custom_openai is a URL, not a place: it points at a paid API as readily as
-- at a local vLLM, and a matched row at 0 reported a real bill as $0.00 with
-- cost_source 'builtin' -- a priced zero, which reads as authoritative,
-- rather than the missing price it actually was. The entry is gone from
-- prices.json as of builtin table version 2, so a fresh install never gets
-- it and those models report cost_source 'none' until an operator adds a
-- price. This removes it from installs that were seeded before that.
--
-- 0011 says a 'builtin' row is refreshed only when the shipped
-- builtin_version grows, and that an operator's edit flips a row to 'user'
-- and is never overwritten by a later upgrade. Both still hold. Seed itself
-- inserts and refreshes and never deletes, deliberately: a general "remove
-- whatever the shipped table stopped listing" rule would let a pattern
-- renamed in some later release silently drop a model's price across every
-- install. Retiring a row is a one-off, numbered, reviewable migration
-- instead -- this one.
--
-- The delete is limited to source = 'builtin'. An operator who edited the
-- catch-all owns it now: their row is 'user', keeps whatever price they set,
-- and is left exactly as it is.
--
-- Rolling back to 0.4.0 brings the row back and this migration does not run
-- again. That binary ships builtin table version 1, which still lists
-- custom_openai '*', so its Seed re-inserts the row as 'builtin'; coming
-- forward again finds 0015 already recorded in schema_migrations and Seed
-- deletes nothing, so the row stays. There is no delete path out of it
-- either: built-in rows cannot be deleted through the API, only edited or
-- reset. The way back is to edit it — PUT /admin/api/prices/{id} flips a
-- built-in row to 'user', so the catch-all can be given the real price of
-- the endpoint it covers even though it cannot be removed. Retiring it a
-- second time would be another numbered migration.
DELETE FROM model_prices
WHERE provider_type = 'custom_openai'
  AND model_pattern = '*'
  AND source = 'builtin';
