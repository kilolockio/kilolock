-- recent_writes.sql
-- Audit: which states were written in the last 24 hours, by whom?
-- The events table captures every state write (and every lock/unlock)
-- with an actor string supplied by the HTTP backend's caller.

SELECT
    e.created_at AT TIME ZONE 'UTC'           AS at_utc,
    s.name                                    AS state_name,
    e.actor                                   AS actor,
    e.kind                                    AS event_kind,
    e.payload ->> 'serial'                    AS serial
FROM   events e
JOIN   states s ON s.id = e.state_id
WHERE  e.created_at >= NOW() - INTERVAL '24 hours'
  AND  e.kind IN ('state_write', 'state_delete')
ORDER  BY e.created_at DESC;
