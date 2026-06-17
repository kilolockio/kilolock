-- drift_current.sql
-- v1.7: what's currently drifted across all my states?
--
-- This is the operator-facing answer the v1.7c demo opens with.
-- The view current_resource_drift surfaces every currently-alive
-- resource whose latest attributes were written by an
-- `kl refresh` and which has NOT been touched by a
-- subsequent apply yet (i.e. drift that hasn't been reconciled).
--
-- Lifecycle-precise: `previous_attributes` is the attribute blob
-- that was current immediately before the refresh, not the blob
-- at the most recent apply. To see the full history of drift on a
-- given address, replace `current_resource_drift` with `resources`
-- and filter on (state_id, address) directly.
--
-- Performance: this view backs by an index on
-- (state_id, address, delete_serial) WHERE delete_serial IS NOT NULL.
-- Cost is O(currently-drifted-rows), not O(state size). Compare
-- this to the raw-tfstate workflow: download the full state file,
-- parse JSON, diff against a snapshot.

WITH changed_keys AS (
    SELECT
        d.resource_id,
        jsonb_agg(k ORDER BY k) AS keys
    FROM current_resource_drift d,
         LATERAL (
             -- Keys present in the current attributes whose value
             -- differs from (or is missing in) the previous blob.
             SELECT key AS k
             FROM   jsonb_each(d.current_attributes)
             WHERE  value IS DISTINCT FROM (d.previous_attributes -> key)
             UNION
             -- Keys removed (present in previous, gone in current).
             SELECT key
             FROM   jsonb_each(d.previous_attributes)
             WHERE  NOT (d.current_attributes ? key)
         ) sub
    GROUP BY d.resource_id
)
SELECT
    d.state_name,
    d.address,
    d.type,
    d.detected_at_serial,
    d.detected_at AT TIME ZONE 'UTC' AS detected_at_utc,
    COALESCE(c.keys, '[]'::jsonb)    AS changed_keys
FROM   current_resource_drift d
LEFT   JOIN changed_keys c ON c.resource_id = d.resource_id
ORDER  BY d.detected_at DESC, d.state_name, d.address
LIMIT  200;
