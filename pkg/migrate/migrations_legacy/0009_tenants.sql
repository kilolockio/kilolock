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

BEGIN;

-- ---------------------------------------------------------------------------
-- tenants: one row per logical customer. In self-hosted mode this
-- table has exactly one row (the singleton). In hosted/SaaS mode
-- it's the root of authentication & RBAC; the auth layer resolves
-- request → tenant_id and every store query filters by that id.
-- ---------------------------------------------------------------------------
CREATE TABLE tenants (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    slug         text        NOT NULL UNIQUE,
    name         text        NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

INSERT INTO tenants (id, slug, name)
VALUES ('00000000-0000-0000-0000-000000000000', 'self-hosted', 'Self-hosted (default)');

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

COMMIT;
