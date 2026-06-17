-- 0015_fix_reservation_unique_constraint.sql
-- Corrects the unique constraint on the resource_reservations table to
-- match the ON CONFLICT clause used by the AcquireReservations function.

BEGIN;

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

COMMIT;