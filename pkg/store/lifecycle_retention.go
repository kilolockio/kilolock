package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type ArchivedTenantPurgeCandidate struct {
	TenantID             string
	TenantSlug           string
	LifecycleChangedAt   time.Time
	EnvironmentCount     int
	APITokenCount        int
	RevokedAPITokenCount int
	ActiveAPITokenCount  int
}

type ArchivedTenantPurgeResult struct {
	TenantSlug          string
	DeletedTenants      int64
	DeletedEnvironments int64
	DeletedAPITokens    int64
}

func (s *Store) ListArchivedTenantPurgeCandidates(ctx context.Context, olderThan time.Time, tenantSlug string) ([]ArchivedTenantPurgeCandidate, error) {
	tenantSlug = strings.TrimSpace(tenantSlug)
	q := `
SELECT t.id::text,
       t.slug,
       t.lifecycle_changed_at,
       (SELECT COUNT(*) FROM environments e WHERE e.tenant_id = t.id) AS env_count,
       (SELECT COUNT(*) FROM api_tokens tok WHERE tok.tenant_id = t.id) AS token_count,
       (SELECT COUNT(*) FROM api_tokens tok WHERE tok.tenant_id = t.id AND tok.revoked_at IS NOT NULL) AS revoked_token_count,
       (SELECT COUNT(*) FROM api_tokens tok WHERE tok.tenant_id = t.id AND tok.revoked_at IS NULL) AS active_token_count
FROM tenants t
WHERE t.lifecycle_status = 'archived'
  AND t.lifecycle_changed_at IS NOT NULL
  AND t.lifecycle_changed_at <= $1`
	args := []any{olderThan}
	if tenantSlug != "" {
		q += ` AND t.slug = $2`
		args = append(args, tenantSlug)
	}
	q += ` ORDER BY t.lifecycle_changed_at ASC, t.slug ASC`
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ArchivedTenantPurgeCandidate
	for rows.Next() {
		var r ArchivedTenantPurgeCandidate
		if err := rows.Scan(
			&r.TenantID,
			&r.TenantSlug,
			&r.LifecycleChangedAt,
			&r.EnvironmentCount,
			&r.APITokenCount,
			&r.RevokedAPITokenCount,
			&r.ActiveAPITokenCount,
		); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) PurgeArchivedTenant(ctx context.Context, tenantSlug string, olderThan time.Time) (ArchivedTenantPurgeResult, error) {
	tenantSlug = strings.TrimSpace(tenantSlug)
	if tenantSlug == "" {
		return ArchivedTenantPurgeResult{}, fmt.Errorf("tenant slug is required")
	}
	res := ArchivedTenantPurgeResult{TenantSlug: tenantSlug}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return res, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var tenantID string
	err = tx.QueryRow(ctx, `
SELECT id::text
FROM tenants
WHERE slug = $1
  AND lifecycle_status = 'archived'
  AND lifecycle_changed_at IS NOT NULL
  AND lifecycle_changed_at <= $2
FOR UPDATE
`, tenantSlug, olderThan).Scan(&tenantID)
	if err != nil {
		return res, err
	}

	tag, err := tx.Exec(ctx, `DELETE FROM api_tokens WHERE tenant_id = $1`, tenantID)
	if err != nil {
		return res, err
	}
	res.DeletedAPITokens = tag.RowsAffected()

	tag, err = tx.Exec(ctx, `DELETE FROM environments WHERE tenant_id = $1`, tenantID)
	if err != nil {
		return res, err
	}
	res.DeletedEnvironments = tag.RowsAffected()

	tag, err = tx.Exec(ctx, `DELETE FROM tenants WHERE id = $1`, tenantID)
	if err != nil {
		return res, err
	}
	res.DeletedTenants = tag.RowsAffected()
	if res.DeletedTenants == 0 {
		return res, fmt.Errorf("tenant %q was not deleted", tenantSlug)
	}
	if err := tx.Commit(ctx); err != nil {
		return res, err
	}
	return res, nil
}
