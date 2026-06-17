-- Add the missing UNIQUE constraint required by the AcquireReservations
-- ON CONFLICT clause. This constraint ensures that an idempotent re-acquire
-- of the same reservation by the same apply run can use DO UPDATE.
ALTER TABLE resource_reservations
ADD CONSTRAINT res_no_self_overlap UNIQUE (state_id, address_glob, holder, apply_id);