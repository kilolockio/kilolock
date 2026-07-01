-- Speed up state-engine scope expansion on large states.
--
-- Current hot path:
--   SELECT ... FROM resources
--   WHERE state_id = $1 AND delete_serial IS NULL
--   ORDER BY address
--
-- The existing partial index on (state_id) WHERE delete_serial IS NULL helps
-- prune to live rows, but it does not help the ORDER BY address shape. This
-- composite partial index lets Postgres walk the current/live resources of one
-- state in address order directly, which is especially helpful once states
-- grow into 100k+ resource territory.

CREATE INDEX IF NOT EXISTS resources_state_open_address_idx
    ON resources (state_id, address)
    WHERE delete_serial IS NULL;
