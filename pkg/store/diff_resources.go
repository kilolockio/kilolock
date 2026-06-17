package store

import (
	"context"
	"encoding/json"
	"fmt"
)

// ResourceAttrDelta is the per-address payload of an attribute-level
// diff between two state versions. The store stops at "here are the
// attribute blobs and the sensitive-path projections for both sides";
// computing the JSON path-level delta (added / removed / changed
// leaves) and rendering it is the CLI's job. This split keeps the
// store layer JSON-agnostic and lets the renderer carry both
// machine-readable (json) and human-readable (table / unified) output
// modes without a second round trip to the database.
//
// Status values:
//
//	added    — present in `to`, absent in `from`. FromAttributes is nil.
//	removed  — present in `from`, absent in `to`. ToAttributes is nil.
//	changed  — present in both with byte-distinct attributes blobs.
//
// Sensitive: the two `*Sensitive` fields carry Terraform's own
// `sensitive_paths` projection (a jsonb list of path arrays) from the
// resource row of each side. The renderer must redact any leaf whose
// path appears in EITHER side's list — a value transitioning from
// sensitive to non-sensitive is itself a sensitive transition.
type ResourceAttrDelta struct {
	Address       string
	Status        string
	FromAttrs     json.RawMessage
	ToAttrs       json.RawMessage
	FromSensitive json.RawMessage
	ToSensitive   json.RawMessage
}

// DiffVersionResources returns the attribute-level changes between
// two state versions of the same state. Designed for the
// `kl diff` CLI; rolls up "all the bytes the renderer needs"
// into one query rather than the N+1 of "diff addresses then fetch
// each side per address".
//
// Both version IDs must belong to the same state (the lifecycle
// columns make a cross-state query both expensive and meaningless).
// The caller is expected to have resolved both refs through
// GetVersionRaw/ListVersions on a single state name first.
//
// Semantics match DiffVersionAddresses (which reuses the same
// lifecycle-ranged "alive at serial V" predicate) extended to
// project the attribute blobs:
//
//	alive at V  ≡  create_serial <= V.serial
//	                 AND (delete_serial IS NULL OR delete_serial > V.serial)
//
// Rows where both sides are identical bytes are filtered out at the
// SQL layer to keep the wire payload bounded by the size of the
// change-set rather than the size of the state.
//
// Output is sorted by address for stable rendering across runs.
func (s *Store) DiffVersionResources(ctx context.Context, fromVersionID, toVersionID string) ([]ResourceAttrDelta, error) {
	if fromVersionID == "" || toVersionID == "" {
		return nil, fmt.Errorf("DiffVersionResources: both version IDs are required")
	}

	const q = `
		WITH vfrom AS (SELECT state_id, serial FROM state_versions WHERE id = $1),
		     vto   AS (SELECT state_id, serial FROM state_versions WHERE id = $2),
		     a AS (
		         SELECT r.address, r.attributes, r.sensitive_paths
		         FROM   resources r, vfrom v
		         WHERE  r.state_id      = v.state_id
		           AND  r.create_serial <= v.serial
		           AND  (r.delete_serial IS NULL OR r.delete_serial > v.serial)
		     ),
		     b AS (
		         SELECT r.address, r.attributes, r.sensitive_paths
		         FROM   resources r, vto v
		         WHERE  r.state_id      = v.state_id
		           AND  r.create_serial <= v.serial
		           AND  (r.delete_serial IS NULL OR r.delete_serial > v.serial)
		     )
		SELECT
		    COALESCE(a.address, b.address) AS address,
		    CASE
		        WHEN a.address IS NULL THEN 'added'
		        WHEN b.address IS NULL THEN 'removed'
		        ELSE 'changed'
		    END AS status,
		    a.attributes,
		    b.attributes,
		    a.sensitive_paths,
		    b.sensitive_paths
		FROM a
		FULL OUTER JOIN b USING (address)
		WHERE a.address IS NULL
		   OR b.address IS NULL
		   OR a.attributes::text IS DISTINCT FROM b.attributes::text
		ORDER BY address
	`
	rows, err := s.pool.Query(ctx, q, fromVersionID, toVersionID)
	if err != nil {
		return nil, fmt.Errorf("diff resource attributes: %w", err)
	}
	defer rows.Close()

	var out []ResourceAttrDelta
	for rows.Next() {
		var d ResourceAttrDelta
		if err := rows.Scan(
			&d.Address, &d.Status,
			&d.FromAttrs, &d.ToAttrs,
			&d.FromSensitive, &d.ToSensitive,
		); err != nil {
			return nil, fmt.Errorf("scan diff resource: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate diff rows: %w", err)
	}
	return out, nil
}
