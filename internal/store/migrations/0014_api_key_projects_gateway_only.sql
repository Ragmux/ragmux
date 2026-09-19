-- api_key_projects is a gateway-key table. Make the schema say so.
--
-- 0010 already refuses a management key a default project
-- (api_keys_mgmt_has_no_project) and limits of its own
-- (api_keys_mgmt_has_no_limits), but it left the grant rows to the
-- application: the branch in internal/admin/keys.go that answers "a
-- management key has no projects" is the only thing standing between a
-- management key and a project routing table. Two thirds of one invariant are
-- declarative and the last third is one `if` in one handler. This closes it.
--
-- A composite foreign key rather than a trigger. It is checked by the planner
-- on the two statements that write grants, there is no procedural code to
-- review or to keep in step with the CHECK constraints next to it, and unlike
-- a trigger it cannot be switched off for a session. The kind column is
-- filled by its DEFAULT, so no query has to name it.

-- Grants held by a management key are already unreachable: LookupGatewayKey
-- resolves a credential only WHERE kind = 'gateway', so nothing ever reads
-- them. Dropping them costs no routing, and it is what lets the constraint
-- below be added to a database that somehow collected one.
DELETE FROM api_key_projects p
      USING api_keys k
      WHERE k.id = p.api_key_id AND k.kind <> 'gateway';

-- The referenced side of a composite foreign key needs a unique constraint
-- covering exactly those columns. id is already the primary key, so this adds
-- an index that can never be larger than api_keys, a table with one row per
-- issued credential.
ALTER TABLE api_keys DROP CONSTRAINT IF EXISTS api_keys_id_kind;
ALTER TABLE api_keys ADD CONSTRAINT api_keys_id_kind UNIQUE (id, kind);

-- kind is a copy of the owning key's, pinned to 'gateway' by the CHECK. The
-- foreign key below then has only one row it can point at: the api_keys row
-- with this id AND kind 'gateway'. A management key has no such row.
ALTER TABLE api_key_projects
    ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT 'gateway';
ALTER TABLE api_key_projects DROP CONSTRAINT IF EXISTS api_key_projects_gateway_only;
ALTER TABLE api_key_projects
    ADD CONSTRAINT api_key_projects_gateway_only CHECK (kind = 'gateway');

-- Replaces the single-column foreign key 0010 created inline. Same
-- ON DELETE CASCADE, so deleting a key still takes its grants with it; what
-- is new is that the referenced row has to be a gateway key. ON UPDATE stays
-- NO ACTION on purpose: nothing rewrites api_keys.kind, and if anything ever
-- tried, refusing the update on a key that holds grants is the right answer.
ALTER TABLE api_key_projects DROP CONSTRAINT IF EXISTS api_key_projects_api_key_id_fkey;
ALTER TABLE api_key_projects DROP CONSTRAINT IF EXISTS api_key_projects_key_is_gateway;
ALTER TABLE api_key_projects
    ADD CONSTRAINT api_key_projects_key_is_gateway
    FOREIGN KEY (api_key_id, kind) REFERENCES api_keys(id, kind) ON DELETE CASCADE;
