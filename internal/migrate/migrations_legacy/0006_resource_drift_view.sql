-- 0006_resource_drift_view.sql
-- v1.7b: the SQL surface for drift. Companion to v1.7a, which
-- exposes per-resource drift addresses in `refresh.Result` and the
-- CLI summary. v1.7b answers the operator's standing question --
-- "what is currently drifted across all of my state, irrespective
-- of which refresh run reported it?" -- in milliseconds, no JSON
-- walk required.
--
-- This is the demo angle for Kilolock vs raw .tfstate: with a
-- flat state file, "what drifted" requires downloading the whole
-- blob, parsing it, and writing custom diff logic against a
-- previous version. With Kilolock it is a SELECT against an
-- indexed view that lives next to your relational metadata.
--
-- Model
-- -----
-- ADR 0004's content-addressable lifecycle model already encodes
-- drift naturally:
--
--   * A resource that survives unchanged across N versions has
--     exactly one row with delete_serial IS NULL.
--   * A resource whose attributes diverge between two versions
--     produces TWO rows for the same address: the prior lifecycle
--     closes (delete_serial = new_serial) and a new lifecycle
--     opens (create_serial = new_serial). v1.6b's refresh
--     orchestrator drives this same path for cloud-side drift via
--     state_versions.source = 'refresh'.
--
-- So "currently drifted" = "this resource's currently-open lifecycle
-- was opened by a refresh, replacing a prior lifecycle, AND no
-- subsequent apply has visited this state". The second clause makes
-- the view match operator intuition: once `terraform apply` has run,
-- the operator has had a chance to reconcile, so the drift row is no
-- longer "pending attention". Without this clause, an apply that
-- happens to re-assert the refresh-discovered value (same content
-- hash → existing lifecycle stays open) would leave a stale drift
-- row indefinitely. The v1.7c demo script exercises exactly this
-- predicate.

BEGIN;

-- ---------------------------------------------------------------------------
-- current_resource_drift (VIEW): every currently-alive resource whose
-- attributes diverged from prior state because of a refresh-sourced
-- write. Lifecycle-precise: the previous_attributes column is the
-- attribute blob that was current immediately before this drift
-- event, not the attribute blob at the latest apply.
--
-- Columns mirror current_resources for the resource identity tuple
-- (so SQL written against current_resources extends naturally), then
-- add the drift-specific fields: previous_attributes, the
-- refresh_run that detected the drift, and detected_at timestamps.
-- ---------------------------------------------------------------------------
CREATE VIEW current_resource_drift AS
SELECT
    r.id                AS resource_id,
    r.state_id,
    s.name              AS state_name,
    r.address,
    r.type,
    r.mode,
    r.module_path,
    r.attributes        AS current_attributes,
    prev.attributes     AS previous_attributes,
    r.create_serial     AS detected_at_serial,
    sv.id               AS detected_in_version_id,
    sv.created_at       AS detected_at,
    rr.id               AS refresh_run_id
FROM   resources r
JOIN   states s
       ON  s.id = r.state_id
JOIN   state_versions sv
       ON  sv.state_id = r.state_id
       AND sv.serial   = r.create_serial
       AND sv.source   = 'refresh'
-- INNER LATERAL: only rows where a prior lifecycle was closed at
-- exactly this resource's create_serial qualify as drift. Brand-new
-- resources (no prior row) cannot be the result of refresh anyway
-- (refresh does not import), but the inner join makes the predicate
-- explicit and self-documenting.
JOIN LATERAL (
    SELECT attributes
    FROM   resources p
    WHERE  p.state_id      = r.state_id
      AND  p.address       = r.address
      AND  p.delete_serial = r.create_serial
    ORDER BY p.create_serial DESC
    LIMIT  1
) prev ON true
-- LEFT JOIN refresh_runs so the view still emits rows when an
-- audit row got pruned, but populates the run id when it survives.
LEFT JOIN refresh_runs rr
       ON  rr.to_version_id = sv.id
WHERE  r.delete_serial IS NULL
  -- "No subsequent apply has reconciled this state": pending drift
  -- only. Without this, an apply re-asserting the refresh-detected
  -- value (same content hash → no new lifecycle) leaves a stale
  -- drift row visible forever.
  AND  NOT EXISTS (
       SELECT 1
       FROM   state_versions sv_next
       WHERE  sv_next.state_id = r.state_id
         AND  sv_next.serial  > r.create_serial
         AND  sv_next.source   = 'apply'
  );

COMMENT ON VIEW current_resource_drift IS
    'Currently-alive resources whose latest attributes were written by `kl refresh`, replacing a previous lifecycle. Lifecycle-aware diff surface; see ADR 0005 and ADR 0006.';

-- ---------------------------------------------------------------------------
-- Supporting index: hot lookup for the LATERAL "find the lifecycle
-- closed at this exact serial" subquery. The closed-by-serial axis
-- is otherwise served by the resources_state_address_idx, but a
-- dedicated partial index keeps the diff predicate constant-time
-- regardless of how many historical (state_id, address) lifecycles
-- exist.
-- ---------------------------------------------------------------------------
CREATE INDEX IF NOT EXISTS resources_closed_at_idx
    ON resources (state_id, address, delete_serial)
    WHERE delete_serial IS NOT NULL;

INSERT INTO schema_migrations (version) VALUES (6)
    ON CONFLICT (version) DO NOTHING;

COMMIT;
