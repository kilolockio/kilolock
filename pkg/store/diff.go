package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// VersionAddressDiff is the address-level delta between two state
// versions, used to summarize a proposed rollback ("from current to
// target") for the operator before they commit.
//
//   - Added:   addresses present in `to` but not in `from`. After a
//     rollback these reappear in the state.
//   - Removed: addresses present in `from` but not in `to`. After a
//     rollback these vanish from the state — and almost certainly
//     become unmanaged orphans in the real cloud unless the operator
//     also reverts the HCL that created them.
//   - Changed: addresses present in both versions whose attributes
//     jsonb is not byte-identical. Attribute-level rendering of the
//     delta is deliberately deferred to a future `kl diff`
//     command; the count + sample is enough for a rollback prompt.
//
// All three slices are sorted lexicographically for stable rendering.
type VersionAddressDiff struct {
	Added   []string
	Removed []string
	Changed []string
}

// DiffVersionAddresses computes the address-level delta between
// two state versions of the same state. The expected use is
// "what changes between current and the rollback target?" — call
// it with from=current_version_id, to=target_version_id.
//
// Both version IDs must belong to the same state; the function
// does not enforce that (the projection tables make a cross-state
// query expensive and ambiguous). Callers should always resolve
// the IDs through GetVersionRaw / ListVersions on a single state
// name first.
//
// Implementation note (post-migration 0002):
//
// The resources table is content-addressable and lifecycle-ranged:
// one row per unique (state_id, address, attributes_hash,
// create_serial) with an optional delete_serial closing the range.
// To enumerate "resources alive at version V" we filter rows by
//
//	create_serial <= V.serial AND
//	(delete_serial IS NULL OR delete_serial > V.serial).
//
// The diff is a FULL OUTER JOIN on address between the two
// resulting sets, classifying each match into added / removed /
// changed. Indexed access via (state_id, address) and the partial
// "open lifecycle" index from migration 0002 keep this cheap even
// on 100k-resource states.
func (s *Store) DiffVersionAddresses(ctx context.Context, fromVersionID, toVersionID string) (*VersionAddressDiff, error) {
	if fromVersionID == "" || toVersionID == "" {
		return nil, fmt.Errorf("DiffVersionAddresses: both version IDs are required")
	}

	const q = `
		WITH vfrom AS (SELECT state_id, serial FROM state_versions WHERE id = $1),
		     vto   AS (SELECT state_id, serial FROM state_versions WHERE id = $2),
		     a AS (
		         SELECT r.address, r.attributes
		         FROM   resources r, vfrom v
		         WHERE  r.state_id      = v.state_id
		           AND  r.create_serial <= v.serial
		           AND  (r.delete_serial IS NULL OR r.delete_serial > v.serial)
		     ),
		     b AS (
		         SELECT r.address, r.attributes
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
		        WHEN a.attributes::text IS DISTINCT FROM b.attributes::text THEN 'changed'
		        ELSE 'same'
		    END AS classification
		FROM a
		FULL OUTER JOIN b USING (address)
	`
	rows, err := s.pool.Query(ctx, q, fromVersionID, toVersionID)
	if err != nil {
		return nil, fmt.Errorf("diff state versions: %w", err)
	}
	defer rows.Close()

	out := &VersionAddressDiff{}
	for rows.Next() {
		var addr, class string
		if err := rows.Scan(&addr, &class); err != nil {
			return nil, fmt.Errorf("scan diff row: %w", err)
		}
		switch class {
		case "added":
			out.Added = append(out.Added, addr)
		case "removed":
			out.Removed = append(out.Removed, addr)
		case "changed":
			out.Changed = append(out.Changed, addr)
		case "same":
			// no-op
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Sort for stable rendering. The SQL FULL OUTER JOIN doesn't
	// guarantee an order, and operator-facing tools (rollback
	// dry-run, future `kl diff`) need deterministic
	// output for screenshots, audit trails, and scriptable tests.
	sortStrings(out.Added)
	sortStrings(out.Removed)
	sortStrings(out.Changed)
	return out, nil
}

// sortStrings is std-lib slice sort in a tiny wrapper so the diff
// file doesn't have to import "sort" just for this. Kept private
// because the wider package already has higher-leverage sort
// helpers elsewhere.
func sortStrings(s []string) {
	// Insertion sort: O(n²) but n is the diff size, which is the
	// CHANGE-set, not the state size. A rollback touching even a
	// few hundred resources is exceptional; we never want to allocate
	// the sort.Sort scaffold on a 5-row slice. Trades log-n for the
	// straight-line code that's faster on tiny inputs.
	for i := 1; i < len(s); i++ {
		v := s[i]
		j := i - 1
		for j >= 0 && s[j] > v {
			s[j+1] = s[j]
			j--
		}
		s[j+1] = v
	}
}

// (re-export sentinel for the rare caller who needs to handle the
// "no rows returned" case differently from a SQL-level error)
var _ = pgx.ErrNoRows
