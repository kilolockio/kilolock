package store

import (
	"context"
	"time"
)

type IACVersionUsageRow struct {
	TerraformVersion string    `json:"terraform_version"`
	Count            int64     `json:"count"`
	LastSeenAt       time.Time `json:"last_seen_at"`
}

// ListIACVersionUsage returns observed terraform_version distribution from
// state_versions, newest-first by last_seen_at.
func (s *Store) ListIACVersionUsage(ctx context.Context, limit int) ([]IACVersionUsageRow, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	rows, err := s.pool.Query(ctx, `
SELECT COALESCE(NULLIF(terraform_version,''), 'unknown') AS terraform_version,
       COUNT(*)::bigint AS n,
       MAX(created_at) AS last_seen_at
FROM state_versions
GROUP BY 1
ORDER BY last_seen_at DESC, n DESC
LIMIT $1
`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]IACVersionUsageRow, 0, limit)
	for rows.Next() {
		var r IACVersionUsageRow
		if err := rows.Scan(&r.TerraformVersion, &r.Count, &r.LastSeenAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
