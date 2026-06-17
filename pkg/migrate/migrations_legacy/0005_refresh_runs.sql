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

BEGIN;

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

COMMIT;
