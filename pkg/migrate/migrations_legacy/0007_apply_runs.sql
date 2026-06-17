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

BEGIN;

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

COMMIT;
