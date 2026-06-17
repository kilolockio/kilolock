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
