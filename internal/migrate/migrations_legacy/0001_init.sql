-- 0001_init.sql
-- Initial schema for Kilolock v0 (queryable state).
-- See docs/schema.md for design rationale.

BEGIN;

CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- ---------------------------------------------------------------------------
-- states: one logical Terraform state.
-- ---------------------------------------------------------------------------
CREATE TABLE states (
    id                 uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    name               text        NOT NULL UNIQUE,
    lineage            uuid,
    current_version_id uuid,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now()
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

COMMIT;
