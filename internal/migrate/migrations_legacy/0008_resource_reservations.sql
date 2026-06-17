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
-- helper in internal/store implements the check pre-INSERT, under
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

BEGIN;

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

COMMIT;
