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
