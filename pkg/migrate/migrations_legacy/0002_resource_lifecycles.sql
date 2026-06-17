-- 0002_resource_lifecycles.sql
-- Phase 2 of the normalization perf work. See ADR 0004 for the design.
--
-- Replaces the "every state_version owns its own complete set of
-- resources rows" model with a content-addressable lifecycle model:
-- one row per unique (state_id, address, attributes_hash, create_serial),
-- with an optional delete_serial closing the lifecycle range. Each
-- resource that survives unchanged across N state versions is exactly
-- one row in the new schema, not N rows.
--
-- Drops historical normalized data. raw_state on state_versions is
-- preserved (and remains the source of truth for export); historical
-- normalized rows can be rebuilt by re-importing or by a future
-- `kl reindex` command.

BEGIN;

-- ---------------------------------------------------------------------------
-- Drop legacy normalized tables / views. resource_dependencies will be
-- recreated as a VIEW below; resources is recreated with the lifecycle
-- columns; outputs is recreated unchanged (keeps the migration self-
-- contained, since we depend on outputs being empty of orphaned rows).
-- ---------------------------------------------------------------------------
DROP TABLE IF EXISTS resource_dependencies;
DROP TABLE IF EXISTS resources CASCADE;
DROP TABLE IF EXISTS outputs;

-- ---------------------------------------------------------------------------
-- resources: content-addressable, lifecycle-ranged.
-- ---------------------------------------------------------------------------
--
--   A resource is "alive in version V" iff
--      r.state_id = V.state_id
--      AND r.create_serial <= V.serial
--      AND (r.delete_serial IS NULL OR r.delete_serial > V.serial).
--
--   The same logical resource (same address, attributes unchanged across
--   versions N..M) is represented by exactly one row whose lifecycle
--   range is [N, M+1).
-- ---------------------------------------------------------------------------
CREATE TABLE resources (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    state_id          uuid NOT NULL REFERENCES states(id) ON DELETE CASCADE,
    address           text NOT NULL,
    mode              text NOT NULL CHECK (mode IN ('managed', 'data')),
    type              text NOT NULL,
    name              text NOT NULL,
    provider          text NOT NULL,
    module_path       text NOT NULL DEFAULT '',
    index_kind        text NOT NULL DEFAULT 'none'
                          CHECK (index_kind IN ('none', 'int', 'string')),
    index_value       text,
    attributes        jsonb NOT NULL DEFAULT '{}'::jsonb,
    sensitive_paths   jsonb NOT NULL DEFAULT '[]'::jsonb,
    dependencies_raw  jsonb NOT NULL DEFAULT '[]'::jsonb,
    attributes_hash   text  NOT NULL,
    create_serial     bigint NOT NULL,
    delete_serial     bigint,
    created_at        timestamptz NOT NULL DEFAULT now(),
    -- Lifecycle ranges may not overlap for the same (state_id, address);
    -- this isn't directly expressible as a constraint without exclusion
    -- constraints (and btree_gist), so the invariant is enforced in the
    -- write path (see pkg/store/normalize.go).
    UNIQUE (state_id, address, create_serial),
    CHECK (delete_serial IS NULL OR delete_serial > create_serial)
);

CREATE INDEX resources_state_id_idx        ON resources (state_id);
CREATE INDEX resources_state_address_idx   ON resources (state_id, address);
-- Partial index for "open" rows (the common case in the hot read path
-- "what's in the current state"): cheap to maintain, very small.
CREATE INDEX resources_state_open_idx      ON resources (state_id) WHERE delete_serial IS NULL;
CREATE INDEX resources_type_idx            ON resources (type);
CREATE INDEX resources_attributes_gin_idx  ON resources USING gin (attributes);

-- ---------------------------------------------------------------------------
-- outputs: still state_version-scoped. Output counts are O(1) per
-- version in practice, so the lifecycle treatment doesn't pay off.
-- ---------------------------------------------------------------------------
CREATE TABLE outputs (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    state_version_id uuid NOT NULL REFERENCES state_versions(id) ON DELETE CASCADE,
    name             text NOT NULL,
    value            jsonb NOT NULL,
    value_type       jsonb NOT NULL,
    sensitive        boolean NOT NULL DEFAULT false,
    UNIQUE (state_version_id, name)
);

CREATE INDEX outputs_state_version_id_idx ON outputs (state_version_id);

-- ---------------------------------------------------------------------------
-- resource_dependencies (VIEW): per-version edges, computed on demand
-- from each live resource's dependencies_raw array against other live
-- resources at the same serial.
--
-- The view exposes the same columns the old table did (state_version_id,
-- from_resource_id, to_resource_id, kind) so SQL using it does not
-- have to be rewritten -- only queries that previously joined
-- resources.state_version_id need updating.
-- ---------------------------------------------------------------------------
CREATE VIEW resource_dependencies AS
SELECT DISTINCT
    sv.id           AS state_version_id,
    r_from.id       AS from_resource_id,
    r_to.id         AS to_resource_id,
    'unknown'::text AS kind
FROM   state_versions sv
JOIN   resources r_from
       ON  r_from.state_id      = sv.state_id
       AND r_from.create_serial <= sv.serial
       AND (r_from.delete_serial IS NULL OR r_from.delete_serial > sv.serial)
CROSS JOIN LATERAL jsonb_array_elements_text(r_from.dependencies_raw) AS dep(addr)
JOIN   resources r_to
       ON  r_to.state_id      = sv.state_id
       AND (r_to.address = dep.addr OR starts_with(r_to.address, dep.addr || '['))
       AND r_to.create_serial <= sv.serial
       AND (r_to.delete_serial IS NULL OR r_to.delete_serial > sv.serial)
WHERE  r_from.id <> r_to.id;

-- ---------------------------------------------------------------------------
-- current_resources (VIEW): syntactic sugar for "what's in the current
-- state right now". Used by docs/queries/ and by the SQL-facing CLI.
-- Includes the state name so most ad-hoc queries don't need to join
-- the states table explicitly.
-- ---------------------------------------------------------------------------
CREATE VIEW current_resources AS
SELECT
    r.id, r.state_id, s.name AS state_name,
    r.address, r.mode, r.type, r.name AS resource_name, r.provider,
    r.module_path, r.index_kind, r.index_value,
    r.attributes, r.sensitive_paths, r.dependencies_raw,
    r.attributes_hash, r.create_serial, r.created_at
FROM   resources r
JOIN   states    s ON s.id = r.state_id
WHERE  r.delete_serial IS NULL;

-- ---------------------------------------------------------------------------
-- current_resource_dependencies (VIEW): edges between currently-alive
-- resources, with no version filter -- the most common query target.
-- ---------------------------------------------------------------------------
CREATE VIEW current_resource_dependencies AS
SELECT DISTINCT
    r_from.id       AS from_resource_id,
    r_to.id         AS to_resource_id,
    'unknown'::text AS kind
FROM   resources r_from
CROSS JOIN LATERAL jsonb_array_elements_text(r_from.dependencies_raw) AS dep(addr)
JOIN   resources r_to
       ON  r_to.state_id = r_from.state_id
       AND (r_to.address = dep.addr OR starts_with(r_to.address, dep.addr || '['))
       AND r_to.delete_serial IS NULL
WHERE  r_from.delete_serial IS NULL
  AND  r_from.id <> r_to.id;

INSERT INTO schema_migrations (version) VALUES (2)
    ON CONFLICT (version) DO NOTHING;

COMMIT;
