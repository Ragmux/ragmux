-- Retire the built-in custom_openai '*' price row.
--
-- custom_openai is a URL, not a place: it points at a paid API as readily as
-- at a local vLLM, so a matched row at 0 would report a real bill as $0.00
-- with cost_source 'builtin' -- a priced zero, which reads as authoritative,
-- rather than the missing price it actually is. With no row those models
-- report cost_source 'none' until an operator adds a price.
--
-- On a new installation this migration deletes nothing. model_prices is
-- created by 0011, which ships in the same release as this file, and
-- prices.json has carried no custom_openai '*' entry since builtin table
-- version 2 -- so a database migrated from scratch never holds the row and
-- the DELETE below matches nothing. What it does clean is a database seeded
-- by a pre-release build off the v0.4 branch, back when the shipped table
-- still listed the catch-all: development and test databases, not released
-- installs. It stays a numbered migration so those converge on the state a
-- fresh install starts in, rather than each being corrected by hand.
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
DELETE FROM model_prices
WHERE provider_type = 'custom_openai'
  AND model_pattern = '*'
  AND source = 'builtin';
