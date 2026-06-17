-- 0001_baseline.sql
-- Squashed baseline schema for fresh installs.
-- Squashed baseline schema for fresh installs.
-- Includes legacy migrations plus early control-plane schema (RBAC, portal users,
-- entitlements). This file is the single source of truth for new installs.
--
-- Important: after a baseline ships, treat it as immutable for upgrade
-- semantics. Existing databases never re-run 0001, so follow-up schema changes
-- must land in a new numbered migration (and may optionally also be copied into
-- the baseline for future fresh installs).

BEGIN;

-- ===== 0001_init.sql =====
-- 0001_init.sql
-- Initial schema for Kilolock v0 (queryable state).
-- See docs/schema.md for design rationale.


CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- ---------------------------------------------------------------------------
-- states: one logical Terraform state.
-- ---------------------------------------------------------------------------
CREATE TABLE states (
    id                 uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    name               text        NOT NULL UNIQUE,
    lineage            uuid,
    current_version_id uuid,
    lifecycle_status   text        NOT NULL DEFAULT 'active',
    lifecycle_changed_at timestamptz,
    lifecycle_changed_by text      NOT NULL DEFAULT '',
    lifecycle_reason   text        NOT NULL DEFAULT '',
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    CHECK (lifecycle_status IN ('active', 'suspended', 'archived'))
);

-- ---------------------------------------------------------------------------
-- state_versions: immutable, append-only history of state writes.
-- ---------------------------------------------------------------------------
CREATE TABLE state_versions (
    id                uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    state_id          uuid        NOT NULL REFERENCES states(id) ON DELETE CASCADE,
    serial            bigint      NOT NULL,
    terraform_version text,
    raw_state         jsonb       NOT NULL,
    source            text        NOT NULL DEFAULT 'unknown',
    created_at        timestamptz NOT NULL DEFAULT now(),
    created_by        text,
    UNIQUE (state_id, serial)
);

CREATE INDEX state_versions_state_serial_desc_idx
    ON state_versions (state_id, serial DESC);

ALTER TABLE states
    ADD CONSTRAINT states_current_version_fk
    FOREIGN KEY (current_version_id) REFERENCES state_versions(id)
    DEFERRABLE INITIALLY DEFERRED;

-- ---------------------------------------------------------------------------
-- resources: one row per resource instance (post for_each / count expansion).
-- ---------------------------------------------------------------------------
CREATE TABLE resources (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    state_version_id  uuid NOT NULL REFERENCES state_versions(id) ON DELETE CASCADE,
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
    UNIQUE (state_version_id, address)
);

CREATE INDEX resources_type_idx              ON resources (type);
CREATE INDEX resources_state_version_id_idx  ON resources (state_version_id);
CREATE INDEX resources_provider_idx          ON resources (provider);
CREATE INDEX resources_attributes_gin_idx    ON resources USING gin (attributes);

-- ---------------------------------------------------------------------------
-- resource_dependencies: normalized adjacency for graph traversal.
-- Per docs/schema.md: this is the projection of dependencies_raw used for
-- recursive CTE traversal. The raw form on resources remains authoritative
-- for round-trip export.
-- ---------------------------------------------------------------------------
CREATE TABLE resource_dependencies (
    from_resource_id uuid NOT NULL REFERENCES resources(id) ON DELETE CASCADE,
    to_resource_id   uuid NOT NULL REFERENCES resources(id) ON DELETE CASCADE,
    kind             text NOT NULL DEFAULT 'unknown'
                          CHECK (kind IN ('unknown', 'explicit', 'implicit')),
    PRIMARY KEY (from_resource_id, to_resource_id)
);

CREATE INDEX resource_dependencies_to_idx ON resource_dependencies (to_resource_id);

-- ---------------------------------------------------------------------------
-- outputs: state-level outputs.
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
-- state_locks: HTTP backend LOCK / UNLOCK semantics. One row per state.
-- v0 enforces whole-state locks only; subgraph locks come in v1+.
-- ---------------------------------------------------------------------------
CREATE TABLE state_locks (
    state_id    uuid        PRIMARY KEY REFERENCES states(id) ON DELETE CASCADE,
    lock_id     text        NOT NULL,
    info        text,
    who         text,
    version     text,
    created     text,
    path        text,
    acquired_at timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz
);

CREATE INDEX state_locks_expires_at_idx ON state_locks (expires_at)
    WHERE expires_at IS NOT NULL;

-- ---------------------------------------------------------------------------
-- events: append-only audit trail.
-- ---------------------------------------------------------------------------
CREATE TABLE events (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    kind             text NOT NULL,
    state_id         uuid REFERENCES states(id) ON DELETE SET NULL,
    state_version_id uuid REFERENCES state_versions(id) ON DELETE SET NULL,
    actor            text,
    payload          jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at       timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX events_state_created_desc_idx
    ON events (state_id, created_at DESC);
CREATE INDEX events_kind_idx ON events (kind);

-- ---------------------------------------------------------------------------
-- Schema version marker. Future migrations bump this row.
--
-- IF NOT EXISTS so the file is safe to re-execute through either path:
-- the docker-entrypoint-initdb.d mount during cluster bootstrap, and
-- the migrate runner against a database where the runner itself has
-- already created the tracking table during readApplied().
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS schema_migrations (
    version    integer     PRIMARY KEY,
    applied_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO schema_migrations (version) VALUES (1)
    ON CONFLICT (version) DO NOTHING;


-- ===== 0002_resource_lifecycles.sql =====
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


-- ===== 0003_provider_schemas.sql =====
-- 0003_provider_schemas.sql
-- v1 step 1: cache provider schemas in Postgres so the refresh path
-- does not pay GetSchema RPC cost on every invocation. See ADR 0005.
--
-- Each row is the full schema as returned by a provider at a specific
-- version, keyed by (provider_source, provider_version). The source
-- is the canonical registry address (e.g. "registry.terraform.io/
-- hashicorp/null"); the version is the resolved semver Terraform's
-- lock file would pin (e.g. "3.3.0"). Together they uniquely identify
-- a provider binary's schema.
--
-- The schema itself is stored as JSONB. The shape mirrors the
-- internal/provider.Schema Go struct (encoding/json round-trip);
-- consumers are expected to decode it via the provider package's
-- helpers, not query the JSONB structure from SQL. That keeps us
-- free to evolve the in-memory schema model without coordinating
-- SQL-level changes.


CREATE TABLE provider_schemas (
    provider_source   TEXT        NOT NULL,
    provider_version  TEXT        NOT NULL,
    protocol_version  SMALLINT    NOT NULL,
    schema_jsonb      JSONB       NOT NULL,
    fetched_at        TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (provider_source, provider_version),

    -- Defensive: this is the wire-level protocol version negotiated
    -- when the schema was fetched. Anything other than 5 or 6 is a
    -- programming error; the check ensures bad data never lands
    -- silently.
    CONSTRAINT provider_schemas_protocol_version_valid
        CHECK (protocol_version IN (5, 6))
);

-- Provider source listing across pinned versions is rare; the
-- primary key already covers exact lookups. No extra indexes needed
-- in v1.1. If `kl providers ls` ever needs to enumerate by
-- source alone, a future commit can add an index on provider_source.

COMMENT ON TABLE  provider_schemas IS 'Cached provider GetSchema responses keyed by (source, version); see ADR 0005.';
COMMENT ON COLUMN provider_schemas.provider_source IS 'Canonical registry address, e.g. registry.terraform.io/hashicorp/null.';
COMMENT ON COLUMN provider_schemas.provider_version IS 'Resolved semver of the provider binary, e.g. 3.3.0.';
COMMENT ON COLUMN provider_schemas.protocol_version IS 'Wire-level plugin protocol negotiated when this schema was fetched (5 or 6).';
COMMENT ON COLUMN provider_schemas.schema_jsonb IS 'JSON-encoded provider.Schema as defined in internal/provider/client.go. Opaque to SQL.';

INSERT INTO schema_migrations (version) VALUES (3)
    ON CONFLICT (version) DO NOTHING;


-- ===== 0004_provider_configs.sql =====
-- 0004_provider_configs.sql
-- v1 step 2 (5b): persisted provider configuration blocks. Companion
-- to the wire-level Configure RPC added in v1.5a — that commit shipped
-- the ability to call ConfigureProvider but left "where does the
-- config come from" unanswered. This commit answers it.
--
-- Each row is one (provider_source, alias) pair with its attribute
-- map stored as JSONB. The schema deliberately mirrors the storage
-- model used for provider_schemas (0003): JSONB column owned by the
-- Go layer, no SQL-side validation of structure beyond presence and
-- shape (object, non-null). The Go layer marshals/unmarshals via
-- encoding/json; consumers should not query into config_jsonb from
-- SQL.
--
-- Why support an alias column at all in v1?
--
-- Terraform's HCL allows the same provider to be declared multiple
-- times with different configurations:
--
--   provider "aws" { region = "us-east-1" }
--   provider "aws" { alias = "west"; region = "us-west-2" }
--
-- Resources reference these via `provider = aws.west`. State files
-- record the alias as part of the provider address. v1 refresh has
-- to look up the right config for each resource, so the table is
-- keyed on (source, alias) from day one. Most rows will have an
-- empty alias.


CREATE TABLE provider_configs (
    provider_source TEXT        NOT NULL,
    alias           TEXT        NOT NULL DEFAULT '',
    config_jsonb    JSONB       NOT NULL,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (provider_source, alias),

    -- Defensive: a config block is structurally a (possibly empty)
    -- object. Storing arrays, scalars, or null at the top level
    -- would silently break encoding at refresh time; reject early.
    CONSTRAINT provider_configs_is_object
        CHECK (jsonb_typeof(config_jsonb) = 'object')
);

-- Listing all aliases for a given source is the common UX (e.g.
-- `kl provider list hashicorp/aws`); the primary key
-- already supports this via prefix scan. No extra indexes in v1.5b.

COMMENT ON TABLE  provider_configs IS 'Persisted provider configuration blocks keyed by (source, alias); see v1.5 commits.';
COMMENT ON COLUMN provider_configs.provider_source IS 'Canonical registry address, e.g. registry.terraform.io/hashicorp/aws.';
COMMENT ON COLUMN provider_configs.alias IS 'HCL alias (empty string = default/unaliased config).';
COMMENT ON COLUMN provider_configs.config_jsonb IS 'Attribute map for the provider config block. Opaque to SQL; owned by the Go layer.';
COMMENT ON COLUMN provider_configs.updated_at IS 'Last write time; advisory, not used for cache eviction.';

INSERT INTO schema_migrations (version) VALUES (4)
    ON CONFLICT (version) DO NOTHING;


-- ===== 0005_refresh_runs.sql =====
-- 0005_refresh_runs.sql
-- v1 step 3 (6a): audit table for `kl refresh` runs.
-- Companion to v1.6b, which adds the orchestrator that walks a state,
-- calls each provider's ReadResource over RPC, and writes a new
-- state_version with the merged result. v1.6a ships only the storage
-- layer for that audit trail, so the orchestrator commit can land
-- with deterministic Begin/Finish hooks against a real schema.
--
-- One row per refresh attempt. The lifecycle is:
--
--   Begin   inserts a row with status='running', from_version_id set
--           to whatever state_version was current at the moment refresh
--           started. to_version_id is left NULL.
--
--   Finish  updates the same row with status in
--           {succeeded, failed, cancelled}, populates the counters
--           and finished_at timestamp, and sets to_version_id to the
--           new state_version (when the refresh wrote one).
--
-- Errors are summarized at the run level (error_summary) rather than
-- per-resource. A 50k-resource state with a flaky provider could
-- produce thousands of per-resource diagnostics; storing those in a
-- relational audit table would explode the row count without adding
-- value the operator can act on. Per-resource detail belongs in
-- structured logs / future v1.7 drift tables.


CREATE TABLE refresh_runs (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),

    -- The state being refreshed. Cascade because if someone deletes
    -- a state, its refresh history is no longer meaningful.
    state_id        uuid        NOT NULL REFERENCES states(id) ON DELETE CASCADE,

    -- The state_version that was current when refresh started.
    -- Always non-NULL — refresh has nothing to read otherwise.
    from_version_id uuid        NOT NULL REFERENCES state_versions(id),

    -- The state_version refresh produced, if any. NULL while running,
    -- and may stay NULL after a failed run that never committed.
    to_version_id   uuid                 REFERENCES state_versions(id),

    started_at      timestamptz NOT NULL DEFAULT now(),
    finished_at     timestamptz,

    -- Counters. NULL while running; populated on Finish.
    resources_checked int,
    resources_changed int,
    resources_failed  int,

    -- Lifecycle status. CHECK keeps writers honest; it's cheap and
    -- catches the typical "I forgot to update finished_at" bug
    -- because the constraint forces an explicit terminal state.
    status          text        NOT NULL DEFAULT 'running'
        CHECK (status IN ('running', 'succeeded', 'failed', 'cancelled')),

    -- Free-form, operator-readable explanation of why a refresh ended
    -- the way it did. NULL on success. Intended length: a few hundred
    -- bytes; not a transcript.
    error_summary   text,

    actor           text,

    created_at      timestamptz NOT NULL DEFAULT now()
);

-- The dominant query is "show me recent refreshes for this state",
-- which `kl refresh history <state>` will use. ORDER BY
-- started_at DESC LIMIT N is fast with this index.
CREATE INDEX refresh_runs_state_started_idx
    ON refresh_runs (state_id, started_at DESC);

-- Defensive invariants. Both are easy to violate from application
-- code if the lifecycle gets refactored carelessly:
--
--   * A row with finished_at set must also be in a terminal status.
--   * A row in 'running' must not have finished_at populated.
--
-- These check both directions of the same invariant and let the DB
-- catch bugs that would otherwise surface as silently inconsistent
-- audit rows.
ALTER TABLE refresh_runs
    ADD CONSTRAINT refresh_runs_running_has_no_finish
        CHECK ((status = 'running') = (finished_at IS NULL));

COMMENT ON TABLE  refresh_runs IS 'Audit log for `kl refresh` orchestrator runs (v1.6).';
COMMENT ON COLUMN refresh_runs.from_version_id IS 'state_version current when refresh began.';
COMMENT ON COLUMN refresh_runs.to_version_id IS 'state_version produced by refresh, if any. NULL while running or on a failed run that did not commit.';
COMMENT ON COLUMN refresh_runs.resources_checked IS 'Number of resources ReadResource was called on. NULL while running.';
COMMENT ON COLUMN refresh_runs.resources_changed IS 'Number whose attributes differed from prior state. NULL while running.';
COMMENT ON COLUMN refresh_runs.resources_failed IS 'Number that returned a non-empty error/diagnostic from the provider. NULL while running.';
COMMENT ON COLUMN refresh_runs.error_summary IS 'Operator-readable summary on terminal failure; NULL on success.';

INSERT INTO schema_migrations (version) VALUES (5)
    ON CONFLICT (version) DO NOTHING;


-- ===== 0006_resource_drift_view.sql =====
-- 0006_resource_drift_view.sql
-- v1.7b: the SQL surface for drift. Companion to v1.7a, which
-- exposes per-resource drift addresses in `refresh.Result` and the
-- CLI summary. v1.7b answers the operator's standing question --
-- "what is currently drifted across all of my state, irrespective
-- of which refresh run reported it?" -- in milliseconds, no JSON
-- walk required.
--
-- This is the demo angle for Kilolock vs raw .tfstate: with a
-- flat state file, "what drifted" requires downloading the whole
-- blob, parsing it, and writing custom diff logic against a
-- previous version. With Kilolock it is a SELECT against an
-- indexed view that lives next to your relational metadata.
--
-- Model
-- -----
-- ADR 0004's content-addressable lifecycle model already encodes
-- drift naturally:
--
--   * A resource that survives unchanged across N versions has
--     exactly one row with delete_serial IS NULL.
--   * A resource whose attributes diverge between two versions
--     produces TWO rows for the same address: the prior lifecycle
--     closes (delete_serial = new_serial) and a new lifecycle
--     opens (create_serial = new_serial). v1.6b's refresh
--     orchestrator drives this same path for cloud-side drift via
--     state_versions.source = 'refresh'.
--
-- So "currently drifted" = "this resource's currently-open lifecycle
-- was opened by a refresh, replacing a prior lifecycle, AND no
-- subsequent apply has visited this state". The second clause makes
-- the view match operator intuition: once `terraform apply` has run,
-- the operator has had a chance to reconcile, so the drift row is no
-- longer "pending attention". Without this clause, an apply that
-- happens to re-assert the refresh-discovered value (same content
-- hash → existing lifecycle stays open) would leave a stale drift
-- row indefinitely. The v1.7c demo script exercises exactly this
-- predicate.


-- ---------------------------------------------------------------------------
-- current_resource_drift (VIEW): every currently-alive resource whose
-- attributes diverged from prior state because of a refresh-sourced
-- write. Lifecycle-precise: the previous_attributes column is the
-- attribute blob that was current immediately before this drift
-- event, not the attribute blob at the latest apply.
--
-- Columns mirror current_resources for the resource identity tuple
-- (so SQL written against current_resources extends naturally), then
-- add the drift-specific fields: previous_attributes, the
-- refresh_run that detected the drift, and detected_at timestamps.
-- ---------------------------------------------------------------------------
CREATE VIEW current_resource_drift AS
SELECT
    r.id                AS resource_id,
    r.state_id,
    s.name              AS state_name,
    r.address,
    r.type,
    r.mode,
    r.module_path,
    r.attributes        AS current_attributes,
    prev.attributes     AS previous_attributes,
    r.create_serial     AS detected_at_serial,
    sv.id               AS detected_in_version_id,
    sv.created_at       AS detected_at,
    rr.id               AS refresh_run_id
FROM   resources r
JOIN   states s
       ON  s.id = r.state_id
JOIN   state_versions sv
       ON  sv.state_id = r.state_id
       AND sv.serial   = r.create_serial
       AND sv.source   = 'refresh'
-- INNER LATERAL: only rows where a prior lifecycle was closed at
-- exactly this resource's create_serial qualify as drift. Brand-new
-- resources (no prior row) cannot be the result of refresh anyway
-- (refresh does not import), but the inner join makes the predicate
-- explicit and self-documenting.
JOIN LATERAL (
    SELECT attributes
    FROM   resources p
    WHERE  p.state_id      = r.state_id
      AND  p.address       = r.address
      AND  p.delete_serial = r.create_serial
    ORDER BY p.create_serial DESC
    LIMIT  1
) prev ON true
-- LEFT JOIN refresh_runs so the view still emits rows when an
-- audit row got pruned, but populates the run id when it survives.
LEFT JOIN refresh_runs rr
       ON  rr.to_version_id = sv.id
WHERE  r.delete_serial IS NULL
  -- "No subsequent apply has reconciled this state": pending drift
  -- only. Without this, an apply re-asserting the refresh-detected
  -- value (same content hash → no new lifecycle) leaves a stale
  -- drift row visible forever.
  AND  NOT EXISTS (
       SELECT 1
       FROM   state_versions sv_next
       WHERE  sv_next.state_id = r.state_id
         AND  sv_next.serial  > r.create_serial
         AND  sv_next.source   = 'apply'
  );

COMMENT ON VIEW current_resource_drift IS
    'Currently-alive resources whose latest attributes were written by `kl refresh`, replacing a previous lifecycle. Lifecycle-aware diff surface; see ADR 0005 and ADR 0006.';

-- ---------------------------------------------------------------------------
-- Supporting index: hot lookup for the LATERAL "find the lifecycle
-- closed at this exact serial" subquery. The closed-by-serial axis
-- is otherwise served by the resources_state_address_idx, but a
-- dedicated partial index keeps the diff predicate constant-time
-- regardless of how many historical (state_id, address) lifecycles
-- exist.
-- ---------------------------------------------------------------------------
CREATE INDEX IF NOT EXISTS resources_closed_at_idx
    ON resources (state_id, address, delete_serial)
    WHERE delete_serial IS NOT NULL;

INSERT INTO schema_migrations (version) VALUES (6)
    ON CONFLICT (version) DO NOTHING;


-- ===== 0007_apply_runs.sql =====
-- 0007_apply_runs.sql
-- v2 step 1 (v2a): audit table for `kl apply` runs.
--
-- Companion to migration 0008 (resource_reservations); created first
-- because reservations FK back to apply_runs.id. One row per
-- `kl apply` invocation, mirroring how refresh_runs (0005)
-- tracks one row per `kl refresh`.
--
-- Lifecycle:
--
--   Begin    inserts status='running' with source_serial = the trunk
--            serial that planning was based on, committed_serial NULL.
--
--   Finish   updates the row in-place with one of the terminal
--            statuses (committed | failed | aborted), populates
--            counters and finished_at, and (on commit) sets
--            committed_serial to the serial of the new state_version
--            this apply produced.
--
-- The "committed" terminal name (vs refresh_runs' "succeeded") is
-- deliberate: a successful apply produces a committed state version,
-- which is the operator-meaningful outcome and matches Postgres
-- transaction vocabulary. "aborted" is for the SIGKILL / expired-lease
-- case where reservations were reclaimed before the apply could
-- complete; distinguishable from "failed" (apply attempted but
-- terraform/provider returned an error) so the operator UI can
-- differentiate "something exploded" from "your run got pre-empted".
--
-- Like refresh_runs, per-resource error detail goes to structured
-- logs and (later) the apply_run_resources child table; this audit
-- row carries summary-level information only.


CREATE TABLE apply_runs (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),

    -- The state being applied to. Cascade because if the state is
    -- deleted, its apply history is no longer meaningful and the
    -- reservations FK from migration 0008 also cascades.
    state_id        uuid        NOT NULL REFERENCES states(id) ON DELETE CASCADE,

    -- The state_version current when `kl apply` was invoked.
    -- The orchestrator refuses to run if the trunk's serial moves
    -- past this between Begin and the slice-build step (plan
    -- staleness guard from ADR 0007). NULL only briefly during
    -- the Begin transaction; in practice always set on returned rows.
    from_version_id uuid        NOT NULL REFERENCES state_versions(id),

    -- The state_version produced by the commit, if any. NULL while
    -- running, may stay NULL on a failed run that never committed.
    to_version_id   uuid                 REFERENCES state_versions(id),

    -- Plain-text serial for fast queries; redundant with
    -- (SELECT serial FROM state_versions WHERE id = from_version_id)
    -- but worth the duplication because it appears in every WHERE
    -- on this table and the join would be silly.
    source_serial    bigint     NOT NULL,
    committed_serial bigint,

    -- Who triggered this apply. Free-form; matches refresh_runs.actor.
    -- Empty string is stored as NULL via NULLIF on insert.
    actor           text,

    started_at      timestamptz NOT NULL DEFAULT now(),
    finished_at     timestamptz,

    -- Counters populated on Finish.
    resources_planned  int,   -- |write_set| at acquire time
    resources_applied  int,   -- |write_set ∩ commit-set| after merge
    resources_failed   int,   -- non-empty diagnostics from provider

    status          text        NOT NULL DEFAULT 'running'
        CHECK (status IN ('running', 'committed', 'failed', 'aborted')),

    -- Operator-readable summary on terminal failure. NULL on success.
    error_summary   text,

    -- Free-form for whatever the orchestrator wants to record (e.g.
    -- reservation glob list at acquire time, terraform version, plan
    -- file hash). Schema deliberately not constrained so we can iterate
    -- without migrations during v2.
    info            jsonb       NOT NULL DEFAULT '{}'::jsonb,

    created_at      timestamptz NOT NULL DEFAULT now()
);

-- "Show me recent applies for this state" — the dominant query for
-- the upcoming `kl apply history <state>` CLI and the v2d
-- demo assertion that overlapping apply windows exist.
CREATE INDEX apply_runs_state_started_idx
    ON apply_runs (state_id, started_at DESC);

-- "Find still-running applies for this state" — used by the
-- reservation acquire path to surface the holder when a conflict
-- is detected. Partial because the vast majority of rows are
-- terminal and the partial keeps the index tiny.
CREATE INDEX apply_runs_state_running_idx
    ON apply_runs (state_id)
    WHERE status = 'running';

-- Same invariant pair as refresh_runs (migration 0005):
--   * a row with finished_at must be in a terminal status
--   * a row in 'running' must not have finished_at
ALTER TABLE apply_runs
    ADD CONSTRAINT apply_runs_running_has_no_finish
        CHECK ((status = 'running') = (finished_at IS NULL));

-- committed_serial may only be set on rows whose terminal status is
-- 'committed'. Catches the bug where a 'failed' apply somehow stores
-- a committed serial — that would lie to history queries.
ALTER TABLE apply_runs
    ADD CONSTRAINT apply_runs_committed_serial_only_on_commit
        CHECK (committed_serial IS NULL OR status = 'committed');

COMMENT ON TABLE  apply_runs IS 'Audit log for `kl apply` orchestrator runs (v2).';
COMMENT ON COLUMN apply_runs.source_serial IS 'Trunk serial at plan time; orchestrator aborts if trunk advances past this on a read-set address before the slice is built.';
COMMENT ON COLUMN apply_runs.committed_serial IS 'Serial of the state_version produced by this apply. NULL until commit; set only on status=committed.';
COMMENT ON COLUMN apply_runs.status IS 'running | committed | failed | aborted. aborted is reserved for lease-expiry / SIGKILL pre-emption.';

INSERT INTO schema_migrations (version) VALUES (7)
    ON CONFLICT (version) DO NOTHING;


-- ===== 0008_resource_reservations.sql =====
-- 0008_resource_reservations.sql
-- v2 step 2 (v2a): the reservations substrate.
--
-- One row per reserved address (or address pattern, once we add
-- prefix globs in v2b+). An `kl apply` invocation acquires
-- the full set of reservations it needs in a single transaction;
-- if any conflict exists, the whole acquire fails and no rows are
-- inserted. Rows are released en-masse when the owning apply_run
-- terminates (success or failure).
--
-- Mode is a two-value enum: 'read' or 'write'. The conflict matrix
-- (ADR 0007) is:
--
--                existing 'read'    existing 'write'
--   new 'read'   OK                 conflict
--   new 'write'  conflict           conflict
--
-- i.e. a write is exclusive; concurrent reads coexist. The Acquire
-- helper in pkg/store implements the check pre-INSERT, under
-- a single transaction protected by a per-state advisory lock to
-- avoid the classic "both transactions see no conflict, both
-- insert" race that SELECT-then-INSERT would otherwise have.
--
-- address_glob is stored as text and v2a treats it as a LITERAL
-- address (string equality only). Prefix-glob support
-- ("module.web.*") is deliberately deferred to a later patch; the
-- column name reserves the design space so the API can evolve
-- without renaming. The demo (v2d) explicitly enumerates each
-- address, so literal-only is sufficient for the first parallel-
-- apply story.
--
-- Leases (expires_at) implement crash recovery. An apply killed
-- with SIGKILL stops heartbeating; once its expiry passes, the
-- next acquire that conflicts with one of its rows reclaims them
-- by deleting the stale rows before re-checking conflicts. This
-- matches state_locks' current behavior and means a wedged
-- subgraph self-heals after at most one lease interval.
--
-- All reservations for one apply share the same apply_id and are
-- released together by deleting WHERE apply_id = ?. The ON DELETE
-- CASCADE on the FK to apply_runs means that nuking a state also
-- cleans up its in-flight reservations.


CREATE TABLE resource_reservations (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    state_id      uuid        NOT NULL REFERENCES states(id)     ON DELETE CASCADE,
    apply_id      uuid        NOT NULL REFERENCES apply_runs(id) ON DELETE CASCADE,

    address_glob  text        NOT NULL,
    mode          text        NOT NULL
        CHECK (mode IN ('read', 'write')),

    -- Who holds the reservation. Free-form, matches apply_runs.actor;
    -- denormalized here for fast operator-facing conflict messages
    -- (the conflict response prints "held by <actor>" without joining
    -- to apply_runs).
    holder        text        NOT NULL,

    -- Free-form for any orchestrator-visible context (e.g. originating
    -- HCL module, predicted action set). Not used by the conflict
    -- check itself.
    info          jsonb       NOT NULL DEFAULT '{}'::jsonb,

    acquired_at   timestamptz NOT NULL DEFAULT now(),
    -- Lease: rows whose expires_at < now() are stale and may be
    -- reclaimed by the next conflicting acquire. The orchestrator
    -- renews leases on a heartbeat (every ~minute in production).
    expires_at    timestamptz NOT NULL,

    -- Prevent the same apply from accidentally inserting the same
    -- (address, mode) pair twice. Useful for idempotent acquires
    -- (caller can retry without producing duplicate rows).
    CONSTRAINT res_no_self_dup UNIQUE (state_id, address_glob, mode, apply_id)
);

-- Hot path: "are there active reservations on this state that
-- conflict with my want set?" — used by every Acquire call. The
-- partial index over non-expired rows keeps the working set small
-- even after long-running operation history accumulates expired
-- (but not-yet-reclaimed) rows.
CREATE INDEX resource_reservations_state_glob_idx
    ON resource_reservations (state_id, address_glob, mode);

-- "Release everything for this apply" — single bulk DELETE on commit
-- or rollback. Hits this index once and is done.
CREATE INDEX resource_reservations_apply_idx
    ON resource_reservations (apply_id);

-- For janitorial cleanup of stale rows (cron / background goroutine
-- in v2.5). Not used by the hot path but cheap to maintain.
CREATE INDEX resource_reservations_expires_idx
    ON resource_reservations (expires_at)
    WHERE expires_at < 'infinity';

COMMENT ON TABLE  resource_reservations IS 'Row-level locks on resource addresses owned by an in-flight kl apply (v2a).';
COMMENT ON COLUMN resource_reservations.address_glob IS 'Currently treated as a literal Terraform address (e.g. random_id.web). Reserved for future prefix-glob support (module.web.*).';
COMMENT ON COLUMN resource_reservations.mode IS 'read or write; conflicts per the matrix in ADR 0007.';
COMMENT ON COLUMN resource_reservations.expires_at IS 'Lease deadline; rows past expiry may be reclaimed on the next conflicting acquire.';

INSERT INTO schema_migrations (version) VALUES (8)
    ON CONFLICT (version) DO NOTHING;


-- ===== 0009_tenants.sql =====
-- 0009_tenants.sql
-- Multi-tenancy "one-way door" migration. Lands a tenants table and
-- a tenant_id column on every state-scoped table, backfilling
-- everything to a hard-coded singleton "self-hosted" tenant so the
-- application code can continue to function unchanged until the
-- follow-up commit that threads tenant identity through the call
-- chain.
--
-- Why now: retrofitting tenant_id onto a populated multi-customer
-- database is the worst kind of migration (long, locking, easy to
-- corrupt). Doing it while we have one user (the operator running
-- self-hosted) is essentially free. Doing it the day before SaaS
-- launch is catastrophic. ADR 0010 has the full rationale; the
-- TL;DR is "the column has to exist before any second customer
-- writes a single row, even if every row stays in one tenant for
-- now".
--
-- Scope decisions:
--
--   - tenant_id is added to every table that has a state_id OR is
--     logically state-rooted in the audit chain (events). Tables
--     that are global shared artifacts (provider_schemas) or
--     system bookkeeping (schema_migrations) stay untouched.
--
--   - provider_configs is intentionally NOT touched here. It is
--     today a global cache; making it tenant-scoped is a separate
--     security decision (one tenant's AWS creds must not leak to
--     another) and a separate commit. Tracked in the
--     follow-up tenant-aware refactor.
--
--   - The states UNIQUE(name) constraint becomes UNIQUE(tenant_id,
--     name). Different tenants can have a state called "prod" —
--     this is essential for the SaaS shape.
--
--   - The tenant_id default is the well-known self-hosted UUID
--     '00000000-0000-0000-0000-000000000000'. The default lives at
--     the SQL level so a) existing inserts keep working through
--     the migration boundary and b) any future code path that
--     forgets to set tenant_id is at least correctly attributed to
--     the self-hosted tenant instead of an invalid row. Once the
--     code threads tenant explicitly, the default becomes a
--     defense-in-depth measure.


-- ---------------------------------------------------------------------------
-- tenants: one row per logical customer. In self-hosted mode this
-- table has exactly one row (the singleton). In hosted/SaaS mode
-- it's the root of authentication & RBAC; the auth layer resolves
-- request → tenant_id and every store query filters by that id.
-- ---------------------------------------------------------------------------
CREATE TABLE tenants (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id text        NOT NULL UNIQUE DEFAULT ('ws_' || substr(replace(gen_random_uuid()::text, '-', ''), 1, 12)),
    slug         text        NOT NULL UNIQUE,
    name         text        NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

INSERT INTO tenants (id, workspace_id, slug, name)
VALUES ('00000000-0000-0000-0000-000000000000', 'ws_000000000000', 'self-hosted', 'Self-hosted (default)');

-- ---------------------------------------------------------------------------
-- tenant_id on every state-scoped table.
--
-- The pattern for each table:
--   1. ADD COLUMN tenant_id with the singleton default (so existing
--      rows immediately get the right value, no separate UPDATE).
--   2. ADD foreign key to tenants(id). No CASCADE — deleting a
--      tenant should be an explicit, deliberate operator action
--      (likely "soft delete then move data" rather than a hard
--      drop).
--   3. NOT NULL is implicit via the DEFAULT covering all rows.
--   4. CREATE INDEX (tenant_id, [primary lookup column]) so the
--      hot read path stays index-driven once filters land in code.
-- ---------------------------------------------------------------------------

ALTER TABLE states
    ADD COLUMN tenant_id uuid NOT NULL
        DEFAULT '00000000-0000-0000-0000-000000000000'
        REFERENCES tenants(id);

-- The (tenant_id, name) tuple is the new uniqueness unit. Different
-- tenants can call their states whatever they like. The previous
-- bare UNIQUE(name) constraint is dropped — keeping it would break
-- the multi-tenant SaaS invariant on the day a second customer
-- signs up named "prod".
ALTER TABLE states DROP CONSTRAINT states_name_key;
ALTER TABLE states ADD CONSTRAINT states_tenant_name_key UNIQUE (tenant_id, name);

CREATE INDEX states_tenant_idx ON states (tenant_id);

-- state_versions: cheap to keep tenant_id explicit so range
-- queries like "all versions written by tenant X this week" don't
-- have to JOIN states. Cost: 16 bytes per row. Benefit: hot-path
-- filter is index-only.
ALTER TABLE state_versions
    ADD COLUMN tenant_id uuid NOT NULL
        DEFAULT '00000000-0000-0000-0000-000000000000'
        REFERENCES tenants(id);

CREATE INDEX state_versions_tenant_idx ON state_versions (tenant_id);

-- resources is the big one — biggest table by row count on any
-- non-trivial state. Index on (tenant_id, state_id, address) makes
-- the "show me the current resources of tenant X's state Y"
-- query fully index-driven.
ALTER TABLE resources
    ADD COLUMN tenant_id uuid NOT NULL
        DEFAULT '00000000-0000-0000-0000-000000000000'
        REFERENCES tenants(id);

CREATE INDEX resources_tenant_state_idx ON resources (tenant_id, state_id);

-- outputs is small per state (typically <50 rows) so tenant_id is
-- mostly for completeness of the cross-cutting filter. No
-- additional index needed beyond the existing state_version_id
-- one — the access path is always "outputs of a specific state
-- version", which already filters down to one tenant transitively.
ALTER TABLE outputs
    ADD COLUMN tenant_id uuid NOT NULL
        DEFAULT '00000000-0000-0000-0000-000000000000'
        REFERENCES tenants(id);

-- state_locks: one row per locked state. tenant_id duplicates
-- information available via the states FK but pays for itself in
-- lock-table queries that don't want to JOIN.
ALTER TABLE state_locks
    ADD COLUMN tenant_id uuid NOT NULL
        DEFAULT '00000000-0000-0000-0000-000000000000'
        REFERENCES tenants(id);

-- events: outlives its state (state_id is ON DELETE SET NULL), so
-- it MUST carry tenant_id explicitly. Otherwise an event from a
-- deleted state would be unattributable to a tenant and become
-- effectively a privacy leak across the multi-tenant boundary.
ALTER TABLE events
    ADD COLUMN tenant_id uuid NOT NULL
        DEFAULT '00000000-0000-0000-0000-000000000000'
        REFERENCES tenants(id);

CREATE INDEX events_tenant_kind_idx ON events (tenant_id, kind, created_at DESC);

ALTER TABLE refresh_runs
    ADD COLUMN tenant_id uuid NOT NULL
        DEFAULT '00000000-0000-0000-0000-000000000000'
        REFERENCES tenants(id);

CREATE INDEX refresh_runs_tenant_idx ON refresh_runs (tenant_id, started_at DESC);

ALTER TABLE apply_runs
    ADD COLUMN tenant_id uuid NOT NULL
        DEFAULT '00000000-0000-0000-0000-000000000000'
        REFERENCES tenants(id);

CREATE INDEX apply_runs_tenant_idx ON apply_runs (tenant_id, started_at DESC);

ALTER TABLE resource_reservations
    ADD COLUMN tenant_id uuid NOT NULL
        DEFAULT '00000000-0000-0000-0000-000000000000'
        REFERENCES tenants(id);

CREATE INDEX resource_reservations_tenant_idx ON resource_reservations (tenant_id);

-- ---------------------------------------------------------------------------
-- Surface tenant_id in current_resources. This is the most-used
-- view for ad-hoc operator SQL, so the tenant column being directly
-- visible saves a JOIN on every "show me what's in tenant X" query.
-- current_resource_drift and current_resource_dependencies are NOT
-- recreated here: neither view references current_resources (both
-- join `resources` directly), so CASCADE is unnecessary and the
-- existing definitions continue to work without modification.
-- ---------------------------------------------------------------------------
DROP VIEW IF EXISTS current_resources;
CREATE VIEW current_resources AS
SELECT
    r.id, r.state_id, s.name AS state_name,
    r.tenant_id, t.slug AS tenant_slug,
    r.address, r.mode, r.type, r.name AS resource_name, r.provider,
    r.module_path, r.index_kind, r.index_value,
    r.attributes, r.sensitive_paths, r.dependencies_raw,
    r.attributes_hash, r.create_serial, r.created_at
FROM   resources r
JOIN   states    s ON s.id = r.state_id
JOIN   tenants   t ON t.id = r.tenant_id
WHERE  r.delete_serial IS NULL;

INSERT INTO schema_migrations (version) VALUES (9)
    ON CONFLICT (version) DO NOTHING;


-- ===== 0010_state_version_tags.sql =====
-- 0010_state_version_tags.sql
-- Named pointers (tags) at state_versions rows. Operators have
-- repeatedly asked for "let me bookmark THIS version, then roll back
-- to it later without remembering the serial number". The cheapest
-- implementation is a side table that maps (tenant, state, tag_name)
-- to a state_version_id; the state_versions table itself stays
-- immutable + serial-indexed.
--
-- Schema choices:
--
--   - Tags are per-(tenant, state). Two states can both have a tag
--     called "prod-deploy"; two tenants can both have a state called
--     "prod" with their own "prod-deploy" tag. The unique constraint
--     is (tenant_id, state_id, tag).
--
--   - Tag move is an UPDATE on the same UNIQUE row, not a delete+
--     insert. This keeps the row's id stable for any future foreign
--     keys we might want (e.g. audit entries referencing a tag) and
--     keeps a single source of truth for "where does this tag point
--     now". The previous version_id is NOT preserved on the row; the
--     events table is the audit trail for that.
--
--   - description is optional and serves as the operator's
--     human-readable note ("before pulling the prod DB migration").
--     We don't try to constrain its length here; the CLI clamps it.
--
--   - actor is required and recorded at SET time. UPDATEs (tag
--     moves) overwrite it with the new mover's identity; the audit
--     trail in events keeps the lineage.
--
--   - on delete cascade for state_id: when a state is deleted, its
--     tags vanish with it. Tags are not a global namespace.
--     state_version_id is ON DELETE RESTRICT (not cascade) — we
--     never delete state_versions today, and if we ever do (e.g. a
--     "vacuum old history" command), we want the tag to block that
--     so the operator notices.
--
--   - tenant_id default mirrors the convention from 0009: the
--     self-hosted singleton UUID, so existing code paths that don't
--     set tenant_id explicitly land on the right row. Once 100% of
--     the call chain threads tenant via context, the default can be
--     dropped — same trajectory as the other state-scoped tables.
--
--   - tag is text-typed (not citext); operators have explicitly
--     reported wanting case-sensitive tag names (so e.g. "PROD" and
--     "prod" can coexist on the rare cross-environment-staging
--     state). If that turns out to be the wrong call we can change
--     the column to citext in a follow-up; LOWER-then-compare on
--     SELECT is easy to add but hard to remove without a data
--     migration once tags exist.

CREATE TABLE state_version_tags (
    id                uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id         uuid        NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000'
                                   REFERENCES tenants(id),
    state_id          uuid        NOT NULL
                                   REFERENCES states(id) ON DELETE CASCADE,
    state_version_id  uuid        NOT NULL
                                   REFERENCES state_versions(id) ON DELETE RESTRICT,
    tag               text        NOT NULL,
    description       text        NULL,
    actor             text        NOT NULL DEFAULT 'unknown',
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);

-- The (state, tag) uniqueness is the meaningful constraint; tenant is
-- already entailed by state. Defining it as (tenant_id, state_id, tag)
-- mirrors the rest of the multi-tenancy schema and gives postgres a
-- usable index for tenant-scoped scans.
CREATE UNIQUE INDEX state_version_tags_state_tag_uidx
    ON state_version_tags (tenant_id, state_id, tag);

-- Look up by version (for join-back from history listings, where we
-- want "what tags point at THIS version").
CREATE INDEX state_version_tags_version_idx
    ON state_version_tags (state_version_id);

-- Look up by tenant (for "show me all my tags across all states").
CREATE INDEX state_version_tags_tenant_idx
    ON state_version_tags (tenant_id);

-- Reserved-name sanity: a tag named 'current' would collide with the
-- ref-resolution alias 'current' in versionLookupQuery. The CHECK
-- runs at INSERT/UPDATE time, before the join logic ever sees the
-- ambiguity. Same applies to '@'-prefixed names which collide with
-- the @N relative-ref shape, and pure-decimal names which collide
-- with serial lookup.
ALTER TABLE state_version_tags
    ADD CONSTRAINT state_version_tags_no_reserved_names
    CHECK (
        tag !~ '^@'                 -- no '@1', '@2', etc.
        AND tag !~ '^[0-9]+$'        -- no '42'
        AND tag <> 'current'         -- explicit alias collision
        AND tag <> ''                -- empty string
        AND length(tag) <= 64        -- generous; mostly to keep CLI columns readable
    );

-- ===== 0011_optimistic_locks.sql =====
-- Migration 0011: optimistic concurrent locks
--
-- The HTTP-backend lock channel only carries a generic LockInfo (who,
-- operation, when) — no resource-level information. To support the
-- "two engineers, same state, disjoint resources, run concurrently"
-- pitch with plain `terraform apply`, the conflict check has to move
-- from LOCK-time (exclusive mutex) to POST-time (write-set diff).
--
-- This migration is the storage half of that change:
--
--   1. state_locks goes from one-row-per-state to many-rows-per-state.
--      Each concurrent terraform invocation holds its own row, keyed
--      by its lock_id, so multiple operators can hold "soft" locks at
--      the same time. Final arbitration happens at commit time inside
--      writeStateInternal.
--
--   2. Each lock row records the trunk serial the operator originally
--      read, so the conflict check at POST can scope its diff to "what
--      committed since this operator started" rather than "what's
--      different since the dawn of time."
--
--   3. states.exclusive_locks is the per-state escape hatch for teams
--      who want vanilla single-writer semantics on a state (sensitive
--      migrations, hand-on-tiller maintenance windows, etc.). Default
--      false — the new optimistic behavior is on by default everywhere.
--
-- The migration is backward-compatible: existing rows already satisfy
-- the new PK (every row has a unique lock_id today because the old PK
-- on state_id forced exactly one row per state). source_serial is
-- nullable; pre-existing locks default to NULL and the store falls
-- back to exclusive semantics for that lock until it's released and
-- reacquired.

-- Drop the old primary key (state_id alone) and add the wider one.
-- We don't touch the row contents — every existing row's (state_id,
-- lock_id) tuple is necessarily unique because the old PK on state_id
-- forbade duplicates.
ALTER TABLE state_locks DROP CONSTRAINT state_locks_pkey;
ALTER TABLE state_locks ADD PRIMARY KEY (state_id, lock_id);

-- source_serial records the trunk serial the lock-holder read at
-- acquire time. The optimistic POST path uses this as the "from"
-- side of its diff: only commits AFTER source_serial are candidates
-- for conflicting with this lock-holder's write set.
--
-- Nullable so the migration can be applied to a database with
-- already-held locks; new acquires populate it.
ALTER TABLE state_locks ADD COLUMN source_serial bigint;

-- states.exclusive_locks toggles the per-state behavior:
--
--   false (default): optimistic. Multiple lock-holders allowed; the
--                    POST path arbitrates via write-set diff.
--   true:            vanilla. Single lock-holder; second LOCK gets
--                    423 just like any other HTTP backend.
--
-- DEFAULT false applies to all existing rows so the new behavior is
-- on everywhere from the moment this migration completes. Operators
-- who want the old behavior on a specific state can flip the column
-- directly (via psql; a CLI toggle can be added later if it turns
-- out to be a frequent operation).
ALTER TABLE states ADD COLUMN exclusive_locks boolean NOT NULL DEFAULT false;

-- ===== 0012_api_tokens.sql =====
-- 0012_api_tokens.sql
-- Per-tenant API tokens for HTTP backend authentication.
--
-- Tokens are stored as SHA-256 hashes; the plaintext is shown once at
-- creation time. Each token belongs to exactly one tenant; the store
-- layer always scopes data by tenant_id from the resolved Principal.


CREATE TABLE api_tokens (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    uuid        NOT NULL REFERENCES tenants(id),
    name         text        NOT NULL,
    token_hash   bytea       NOT NULL,
    token_prefix text        NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    revoked_at   timestamptz,
    last_used_at timestamptz,
    CONSTRAINT api_tokens_tenant_name_key UNIQUE (tenant_id, name)
);

-- Globally unique active token hashes (Bearer auth has no tenant hint).
CREATE UNIQUE INDEX api_tokens_active_hash_idx
    ON api_tokens (token_hash)
    WHERE revoked_at IS NULL;

CREATE INDEX api_tokens_tenant_idx ON api_tokens (tenant_id);

INSERT INTO schema_migrations (version) VALUES (12)
    ON CONFLICT (version) DO NOTHING;


-- ===== 0013_environments.sql =====
-- 0013_environments.sql
-- Environment-scoped isolation (ADR 0013). Each environment is a deployable
-- unit; API tokens bind to one environment. database_dsn NULL means "use the
-- server's default connection" (unified / self-hosted mode).


CREATE TABLE environments (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     uuid        NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    env_public_id text        NOT NULL UNIQUE DEFAULT ('env_' || substr(replace(gen_random_uuid()::text, '-', ''), 1, 12)),
    slug          text        NOT NULL,
    tier          text        NOT NULL DEFAULT 'shared_host'
        CHECK (tier IN ('shared_host', 'dedicated_host')),
    status        text        NOT NULL DEFAULT 'ready'
        CHECK (status IN ('provisioning', 'ready', 'failed')),
    database_name text,
    database_dsn  text,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT environments_tenant_slug_key UNIQUE (tenant_id, slug)
);

CREATE INDEX environments_tenant_idx ON environments (tenant_id);
CREATE INDEX environments_status_idx ON environments (status) WHERE status != 'ready';

-- One default environment per existing tenant.
INSERT INTO environments (tenant_id, env_public_id, slug, tier, status, database_name)
SELECT id, 'env_' || substr(replace(id::text, '-', ''), 1, 12), 'default', 'shared_host', 'ready', NULL
FROM   tenants;

ALTER TABLE api_tokens
    ADD COLUMN environment_id uuid REFERENCES environments(id);

UPDATE api_tokens tok
SET    environment_id = e.id
FROM   environments e
WHERE  e.tenant_id = tok.tenant_id
  AND  e.slug = 'default';

ALTER TABLE api_tokens
    ALTER COLUMN environment_id SET NOT NULL;

ALTER TABLE api_tokens DROP CONSTRAINT api_tokens_tenant_name_key;
ALTER TABLE api_tokens
    ADD CONSTRAINT api_tokens_environment_name_key UNIQUE (environment_id, name);

CREATE INDEX api_tokens_environment_idx ON api_tokens (environment_id);

INSERT INTO schema_migrations (version) VALUES (13)
    ON CONFLICT (version) DO NOTHING;


-- ===== 0014_add_reservation_unique_constraint.sql =====
-- Add the missing UNIQUE constraint required by the AcquireReservations
-- ON CONFLICT clause. This constraint ensures that an idempotent re-acquire
-- of the same reservation by the same apply run can use DO UPDATE.
ALTER TABLE resource_reservations
ADD CONSTRAINT res_no_self_overlap UNIQUE (state_id, address_glob, holder, apply_id);

-- ===== 0014_dedicated_host.sql =====
-- 0014_dedicated_host.sql
-- Metadata for dedicated Cloud SQL instances (ADR 0013 E6).


ALTER TABLE environments
    ADD COLUMN host_connection_name text,
    ADD COLUMN source_database_dsn  text,
    ADD COLUMN provision_error      text,
    ADD COLUMN provision_started_at timestamptz,
    ADD COLUMN provision_finished_at timestamptz;

CREATE INDEX environments_provisioning_idx
    ON environments (status, tier)
    WHERE status = 'provisioning';

INSERT INTO schema_migrations (version) VALUES (14)
    ON CONFLICT (version) DO NOTHING;


-- ===== 0015_fix_reservation_unique_constraint.sql =====
-- 0015_fix_reservation_unique_constraint.sql
-- Corrects the unique constraint on the resource_reservations table to
-- match the ON CONFLICT clause used by the AcquireReservations function.


-- Drop the incorrect unique constraint from migration 0008.
-- The original constraint included `mode`, which prevents an idempotent
-- re-acquire from upgrading a 'read' lock to a 'write' lock.
ALTER TABLE resource_reservations DROP CONSTRAINT IF EXISTS res_no_self_dup;

-- Also drop the target constraint in case a previous failed migration
-- run created it without recording the migration as complete. This
-- makes the migration idempotent.
ALTER TABLE resource_reservations DROP CONSTRAINT IF EXISTS res_no_self_overlap;

-- Add the correct UNIQUE constraint required by the AcquireReservations ON CONFLICT clause.
ALTER TABLE resource_reservations
ADD CONSTRAINT res_no_self_overlap UNIQUE (state_id, address_glob, holder, apply_id);

INSERT INTO schema_migrations (version) VALUES (15)
    ON CONFLICT (version) DO NOTHING;


-- ===== 0016_environment_instance_key.sql =====
-- 0016_environment_instance_key.sql
-- Adds data-plane instance routing key for environments.


ALTER TABLE environments
    ADD COLUMN IF NOT EXISTS database_instance_key text;

UPDATE environments
SET    database_instance_key = 'shared'
WHERE  NULLIF(TRIM(COALESCE(database_instance_key, '')), '') IS NULL;

UPDATE environments
SET    database_instance_key = host_connection_name
WHERE  tier = 'dedicated_host'
  AND  NULLIF(TRIM(COALESCE(host_connection_name, '')), '') IS NOT NULL
  AND  database_instance_key = 'shared';

ALTER TABLE environments
    ALTER COLUMN database_instance_key SET DEFAULT 'shared';

ALTER TABLE environments
    ALTER COLUMN database_instance_key SET NOT NULL;

CREATE INDEX IF NOT EXISTS environments_instance_key_idx
    ON environments (database_instance_key);

INSERT INTO schema_migrations (version) VALUES (16)
    ON CONFLICT (version) DO NOTHING;


-- ===== 0017_environment_migration_status.sql =====
-- 0017_environment_migration_status.sql
-- Tracks per-environment migration outcomes for resilient startup.


ALTER TABLE environments
    ADD COLUMN IF NOT EXISTS last_migration_version integer,
    ADD COLUMN IF NOT EXISTS last_migration_at timestamptz,
    ADD COLUMN IF NOT EXISTS last_migration_error text;

CREATE INDEX IF NOT EXISTS environments_last_migration_at_idx
    ON environments (last_migration_at);

INSERT INTO schema_migrations (version) VALUES (17)
    ON CONFLICT (version) DO NOTHING;


-- ===== 0018_system_init.sql =====
CREATE TABLE IF NOT EXISTS system_init (
    singleton      boolean PRIMARY KEY DEFAULT TRUE CHECK (singleton),
    initialized    boolean NOT NULL DEFAULT FALSE,
    init_mode      text NOT NULL DEFAULT '',
    initialized_at timestamptz,
    initialized_by text NOT NULL DEFAULT '',
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);

INSERT INTO system_init (singleton, initialized, init_mode, initialized_by)
VALUES (TRUE, FALSE, '', '')
ON CONFLICT (singleton) DO NOTHING;

-- ===== 0018-0024 squash: lifecycle + RBAC + portal + entitlements =====

-- Lifecycle status columns for tenants/environments/tokens.
ALTER TABLE tenants
    ADD COLUMN IF NOT EXISTS lifecycle_status text NOT NULL DEFAULT 'active'
    CHECK (lifecycle_status IN ('active', 'suspended', 'archived'));

ALTER TABLE environments
    ADD COLUMN IF NOT EXISTS lifecycle_status text NOT NULL DEFAULT 'active'
    CHECK (lifecycle_status IN ('active', 'suspended', 'archived'));

ALTER TABLE api_tokens
    ADD COLUMN IF NOT EXISTS lifecycle_status text NOT NULL DEFAULT 'active'
    CHECK (lifecycle_status IN ('active', 'suspended', 'archived'));

CREATE INDEX IF NOT EXISTS tenants_lifecycle_status_idx
    ON tenants (lifecycle_status);

CREATE INDEX IF NOT EXISTS environments_lifecycle_status_idx
    ON environments (lifecycle_status);

CREATE INDEX IF NOT EXISTS api_tokens_lifecycle_status_idx
    ON api_tokens (lifecycle_status);

-- Lifecycle audit columns.
ALTER TABLE tenants
    ADD COLUMN IF NOT EXISTS lifecycle_changed_at timestamptz,
    ADD COLUMN IF NOT EXISTS lifecycle_changed_by text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS lifecycle_reason text NOT NULL DEFAULT '';

ALTER TABLE environments
    ADD COLUMN IF NOT EXISTS lifecycle_changed_at timestamptz,
    ADD COLUMN IF NOT EXISTS lifecycle_changed_by text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS lifecycle_reason text NOT NULL DEFAULT '';

ALTER TABLE api_tokens
    ADD COLUMN IF NOT EXISTS lifecycle_changed_at timestamptz,
    ADD COLUMN IF NOT EXISTS lifecycle_changed_by text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS lifecycle_reason text NOT NULL DEFAULT '';

-- RBAC foundation tables + seeded roles/permissions.
CREATE TABLE IF NOT EXISTS rbac_roles (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    key         text        NOT NULL UNIQUE,
    name        text        NOT NULL,
    description text        NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS rbac_permissions (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    key         text        NOT NULL UNIQUE,
    description text        NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS rbac_role_permissions (
    role_id       uuid        NOT NULL REFERENCES rbac_roles(id) ON DELETE CASCADE,
    permission_id uuid        NOT NULL REFERENCES rbac_permissions(id) ON DELETE CASCADE,
    created_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (role_id, permission_id)
);

CREATE TABLE IF NOT EXISTS rbac_principal_roles (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    subject_kind  text        NOT NULL,
    subject_id    text        NOT NULL,
    role_id       uuid        NOT NULL REFERENCES rbac_roles(id) ON DELETE CASCADE,
    scope_kind    text        NOT NULL DEFAULT 'global',
    scope_ref     text        NOT NULL DEFAULT '',
    granted_by    text        NOT NULL DEFAULT '',
    granted_at    timestamptz NOT NULL DEFAULT now(),
    revoked_at    timestamptz,
    CHECK (scope_kind IN ('global', 'tenant', 'environment'))
);

CREATE UNIQUE INDEX IF NOT EXISTS rbac_principal_roles_active_uq
    ON rbac_principal_roles (subject_kind, subject_id, role_id, scope_kind, scope_ref)
    WHERE revoked_at IS NULL;

CREATE INDEX IF NOT EXISTS rbac_principal_roles_subject_idx
    ON rbac_principal_roles (subject_kind, subject_id)
    WHERE revoked_at IS NULL;

INSERT INTO rbac_permissions (key, description) VALUES
    ('tenant.read',              'Read tenant metadata.'),
    ('tenant.create',            'Create tenant.'),
    ('tenant.lifecycle.update',  'Suspend/archive/reactivate tenant.'),
    ('tenant.billing.checkout',  'Create Stripe checkout sessions / manage tenant billing.'),
    ('state.config.update',      'Update per-state concurrency and coexistence policy.'),
    ('state.delete',             'Delete a Terraform state snapshot and its history.'),
    ('environment.read',         'Read environments.'),
    ('environment.create',       'Create environment.'),
    ('environment.lifecycle.update', 'Suspend/archive/reactivate environment.'),
    ('environment.transfer.update', 'Create and resolve environment ownership transfer proposals.'),
    ('environment.provision',    'Run environment provisioning workflows.'),
    ('token.read',               'Read token metadata.'),
    ('token.create',             'Create API tokens.'),
    ('token.lifecycle.update',   'Suspend/archive/reactivate token.'),
    ('retention.purge',          'Execute archived tenant retention purge.'),
    ('rbac.manage',              'Grant/revoke roles and manage RBAC assignments.'),
    ('tenant.entitlements.update', 'Update tenant billing plan and entitlement caps.')
ON CONFLICT (key) DO NOTHING;

INSERT INTO rbac_roles (key, name, description) VALUES
    ('platform_admin', 'Platform admin', 'Full access to manage all tenants and infrastructure.'),
    ('tenant_admin',   'Tenant admin',   'Admin within a tenant scope; can manage envs/tokens and lifecycle.'),
    ('billing_admin',  'Billing admin',  'Can manage billing for a tenant (checkout, payment methods).'),
    ('provisioner',    'Provisioner',    'Can run environment provisioning workflows.'),
    ('support_readonly', 'Support readonly', 'Read-only support visibility.'),
    ('support_admin', 'Support admin', 'Support operator with recovery/lifecycle powers but no billing or RBAC management.'),
    ('security_admin', 'Security admin', 'Security operator who can manage control tokens and RBAC assignments.')
ON CONFLICT (key) DO NOTHING;

WITH pairs(role_key, perm_key) AS (
    VALUES
      -- platform_admin
      ('platform_admin', 'tenant.read'),
      ('platform_admin', 'tenant.create'),
      ('platform_admin', 'tenant.lifecycle.update'),
      ('platform_admin', 'tenant.entitlements.update'),
      ('platform_admin', 'tenant.billing.checkout'),
      ('platform_admin', 'state.config.update'),
      ('platform_admin', 'state.delete'),
      ('platform_admin', 'environment.read'),
      ('platform_admin', 'environment.create'),
      ('platform_admin', 'environment.lifecycle.update'),
      ('platform_admin', 'environment.transfer.update'),
      ('platform_admin', 'environment.provision'),
      ('platform_admin', 'token.read'),
      ('platform_admin', 'token.create'),
      ('platform_admin', 'token.lifecycle.update'),
      ('platform_admin', 'retention.purge'),
      ('platform_admin', 'rbac.manage'),
      -- tenant_admin
      ('tenant_admin', 'tenant.read'),
      ('tenant_admin', 'tenant.lifecycle.update'),
      ('tenant_admin', 'state.config.update'),
      ('tenant_admin', 'state.delete'),
      ('tenant_admin', 'environment.read'),
      ('tenant_admin', 'environment.create'),
      ('tenant_admin', 'environment.lifecycle.update'),
      ('tenant_admin', 'token.read'),
      ('tenant_admin', 'token.create'),
      ('tenant_admin', 'token.lifecycle.update'),
      -- billing_admin
      ('billing_admin', 'tenant.read'),
      ('billing_admin', 'tenant.billing.checkout'),
      -- provisioner
      ('provisioner', 'environment.read'),
      ('provisioner', 'environment.provision'),
      -- support_readonly
      ('support_readonly', 'tenant.read'),
      ('support_readonly', 'environment.read'),
      ('support_readonly', 'token.read'),
      -- support_admin
      ('support_admin', 'tenant.read'),
      ('support_admin', 'state.config.update'),
      ('support_admin', 'state.delete'),
      ('support_admin', 'environment.read'),
      ('support_admin', 'environment.lifecycle.update'),
      ('support_admin', 'token.read'),
      -- security_admin
      ('security_admin', 'tenant.read'),
      ('security_admin', 'environment.read'),
      ('security_admin', 'token.read'),
      ('security_admin', 'token.create'),
      ('security_admin', 'token.lifecycle.update'),
      ('security_admin', 'rbac.manage')
)
INSERT INTO rbac_role_permissions (role_id, permission_id)
SELECT r.id, p.id
FROM pairs
JOIN rbac_roles r ON r.key = pairs.role_key
JOIN rbac_permissions p ON p.key = pairs.perm_key
ON CONFLICT (role_id, permission_id) DO NOTHING;

-- Retention purge audit trail.
CREATE TABLE IF NOT EXISTS retention_purge_audit (
    id bigserial PRIMARY KEY,
    tenant_slug text NOT NULL,
    cutoff_at timestamptz NOT NULL,
    actor text NOT NULL DEFAULT '',
    reason text NOT NULL DEFAULT '',
    apply_mode boolean NOT NULL DEFAULT false,
    status text NOT NULL CHECK (status IN ('dry_run', 'applied', 'failed')),
    deleted_tenants bigint NOT NULL DEFAULT 0,
    deleted_environments bigint NOT NULL DEFAULT 0,
    deleted_api_tokens bigint NOT NULL DEFAULT 0,
    error_message text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS retention_purge_audit_created_idx
    ON retention_purge_audit (created_at DESC);

CREATE INDEX IF NOT EXISTS retention_purge_audit_tenant_idx
    ON retention_purge_audit (tenant_slug, created_at DESC);

-- Portal accounts + tenant memberships + sessions.
CREATE TABLE IF NOT EXISTS portal_accounts (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email          text NOT NULL UNIQUE,
    company        text NOT NULL DEFAULT '',
    plan           text NOT NULL DEFAULT 'starter',
    password_salt  text NOT NULL,
    password_hash  text NOT NULL,
    password_login_enabled boolean NOT NULL DEFAULT true,
    auth_source    text NOT NULL DEFAULT 'password' CHECK (auth_source IN ('password', 'oidc')),
    oidc_provider  text NOT NULL DEFAULT '',
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS tenant_memberships (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_slug    text NOT NULL REFERENCES tenants(slug) ON DELETE CASCADE,
    account_id     uuid NOT NULL REFERENCES portal_accounts(id) ON DELETE CASCADE,
    role           text NOT NULL DEFAULT 'member' CHECK (role IN ('owner', 'tenant_admin', 'billing_admin', 'member')),
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    revoked_at     timestamptz,
    revoked_by     text NOT NULL DEFAULT ''
);

CREATE UNIQUE INDEX IF NOT EXISTS tenant_memberships_active_uq
    ON tenant_memberships (tenant_slug, account_id)
    WHERE revoked_at IS NULL;

CREATE INDEX IF NOT EXISTS tenant_memberships_tenant_idx ON tenant_memberships (tenant_slug, created_at);
CREATE INDEX IF NOT EXISTS tenant_memberships_account_idx ON tenant_memberships (account_id, created_at);

CREATE TABLE IF NOT EXISTS tenant_invitations (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_slug   text NOT NULL REFERENCES tenants(slug) ON DELETE CASCADE,
    email         text NOT NULL,
    role          text NOT NULL DEFAULT 'member' CHECK (role IN ('tenant_admin', 'billing_admin', 'member')),
    status        text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'accepted', 'rejected', 'cancelled', 'expired')),
    invited_by    text NOT NULL DEFAULT '',
    responded_at  timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS tenant_invitations_tenant_idx ON tenant_invitations (tenant_slug, created_at DESC);
CREATE INDEX IF NOT EXISTS tenant_invitations_email_idx ON tenant_invitations (email, created_at DESC);

CREATE TABLE IF NOT EXISTS portal_sessions (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id  uuid NOT NULL REFERENCES portal_accounts(id) ON DELETE CASCADE,
    active_tenant_slug text REFERENCES tenants(slug) ON DELETE SET NULL,
    token_hash  text NOT NULL UNIQUE,
    expires_at  timestamptz NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS portal_sessions_account_idx ON portal_sessions (account_id);
CREATE INDEX IF NOT EXISTS portal_sessions_expires_idx ON portal_sessions (expires_at);

CREATE TABLE IF NOT EXISTS ownership_transfer_proposals (
    id                      uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    resource_type           text NOT NULL CHECK (resource_type IN ('environment')),
    resource_id             uuid NOT NULL,
    resource_name           text NOT NULL DEFAULT '',
    current_owner_kind      text NOT NULL CHECK (current_owner_kind IN ('tenant')),
    current_owner_ref       text NOT NULL,
    target_owner_kind       text NOT NULL CHECK (target_owner_kind IN ('tenant')),
    target_owner_ref        text NOT NULL,
    billing_impact          boolean NOT NULL DEFAULT true,
    status                  text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'accepted', 'rejected', 'cancelled', 'expired')),
    initiated_by_account_id uuid REFERENCES portal_accounts(id) ON DELETE SET NULL,
    initiated_by            text NOT NULL DEFAULT '',
    initiated_reason        text NOT NULL DEFAULT '',
    accepted_by_account_id  uuid REFERENCES portal_accounts(id) ON DELETE SET NULL,
    accepted_by             text NOT NULL DEFAULT '',
    accepted_at             timestamptz,
    rejected_by_account_id  uuid REFERENCES portal_accounts(id) ON DELETE SET NULL,
    rejected_by             text NOT NULL DEFAULT '',
    rejected_at             timestamptz,
    cancelled_by_account_id uuid REFERENCES portal_accounts(id) ON DELETE SET NULL,
    cancelled_by            text NOT NULL DEFAULT '',
    cancelled_at            timestamptz,
    expires_at              timestamptz,
    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS ownership_transfer_proposals_resource_idx
    ON ownership_transfer_proposals (resource_type, resource_id, created_at DESC);
CREATE INDEX IF NOT EXISTS ownership_transfer_proposals_current_owner_idx
    ON ownership_transfer_proposals (current_owner_ref, created_at DESC);
CREATE INDEX IF NOT EXISTS ownership_transfer_proposals_target_owner_idx
    ON ownership_transfer_proposals (target_owner_ref, created_at DESC);

-- Tenant entitlement metadata (billing plan + caps).
ALTER TABLE tenants
    ADD COLUMN IF NOT EXISTS kind text NOT NULL DEFAULT 'organization',
    ADD COLUMN IF NOT EXISTS personal_owner_account_id uuid REFERENCES portal_accounts(id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS billing_plan text NOT NULL DEFAULT 'starter',
    ADD COLUMN IF NOT EXISTS max_environments integer NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS max_state_resources integer NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS max_environment_resources integer NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS stripe_customer_id text,
    ADD COLUMN IF NOT EXISTS stripe_subscription_id text,
    ADD COLUMN IF NOT EXISTS stripe_subscription_status text,
    ADD COLUMN IF NOT EXISTS stripe_price_id text,
    ADD COLUMN IF NOT EXISTS stripe_current_period_end timestamptz,
    ADD COLUMN IF NOT EXISTS stripe_updated_at timestamptz;

CREATE UNIQUE INDEX IF NOT EXISTS tenants_stripe_customer_id_uq
    ON tenants (stripe_customer_id)
    WHERE stripe_customer_id IS NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS tenants_personal_owner_account_uq
    ON tenants (personal_owner_account_id)
    WHERE personal_owner_account_id IS NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS tenants_stripe_subscription_id_uq
    ON tenants (stripe_subscription_id)
    WHERE stripe_subscription_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS stripe_webhook_events (
    id          text        PRIMARY KEY,
    type        text        NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now()
);

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conname = 'tenants_kind_valid'
          AND conrelid = 'tenants'::regclass
    ) THEN
        EXECUTE 'ALTER TABLE tenants ADD CONSTRAINT tenants_kind_valid CHECK (kind IN (''organization'',''personal''))';
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conname = 'tenants_max_environment_resources_positive'
          AND conrelid = 'tenants'::regclass
    ) THEN
        EXECUTE 'ALTER TABLE tenants ADD CONSTRAINT tenants_max_environment_resources_positive CHECK (max_environment_resources >= 0)';
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conname = 'tenants_max_environments_positive'
          AND conrelid = 'tenants'::regclass
    ) THEN
        EXECUTE 'ALTER TABLE tenants ADD CONSTRAINT tenants_max_environments_positive CHECK (max_environments >= 0)';
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conname = 'tenants_max_state_resources_positive'
          AND conrelid = 'tenants'::regclass
    ) THEN
        EXECUTE 'ALTER TABLE tenants ADD CONSTRAINT tenants_max_state_resources_positive CHECK (max_state_resources >= 0)';
    END IF;
END
$$;

COMMIT;
