package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type PortalEnvironmentPATGrantRow struct {
	ID              string
	AccountID       string
	EnvironmentID   string
	EnvironmentSlug string
	EnvironmentName string
	GrantedBy       string
	CreatedAt       time.Time
	RevokedAt       *time.Time
	RevokedBy       string
}

func (s *Store) ListPortalEnvironmentPATGrantsByTenant(ctx context.Context, tenantSlug string) ([]PortalEnvironmentPATGrantRow, error) {
	tenantSlug = strings.TrimSpace(tenantSlug)
	if tenantSlug == "" {
		return nil, fmt.Errorf("tenant slug is required")
	}
	rows, err := s.pool.Query(ctx, `
SELECT g.id::text, g.account_id::text, g.environment_id::text, e.slug, t.name, g.granted_by, g.created_at, g.revoked_at, g.revoked_by
FROM portal_environment_pat_grants g
JOIN environments e ON e.id = g.environment_id
JOIN tenants t ON t.id = e.tenant_id
WHERE t.slug = $1
  AND g.revoked_at IS NULL
ORDER BY e.slug ASC, g.created_at ASC`, tenantSlug)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PortalEnvironmentPATGrantRow
	for rows.Next() {
		var row PortalEnvironmentPATGrantRow
		if err := rows.Scan(&row.ID, &row.AccountID, &row.EnvironmentID, &row.EnvironmentSlug, &row.EnvironmentName, &row.GrantedBy, &row.CreatedAt, &row.RevokedAt, &row.RevokedBy); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func (s *Store) SetPortalEnvironmentPATGrant(ctx context.Context, tenantSlug, environmentSlug, accountID, actor string, allowed bool) error {
	tenantSlug = strings.TrimSpace(tenantSlug)
	environmentSlug = strings.TrimSpace(environmentSlug)
	accountID = strings.TrimSpace(accountID)
	actor = strings.TrimSpace(actor)
	if tenantSlug == "" || environmentSlug == "" || accountID == "" {
		return fmt.Errorf("tenant slug, environment slug, and account id are required")
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var (
			tenantID      string
			environmentID string
			tenantKind    string
			membershipCnt int
		)
		err := tx.QueryRow(ctx, `
SELECT t.id::text, e.id::text, t.kind
FROM tenants t
JOIN environments e ON e.tenant_id = t.id
WHERE t.slug = $1
  AND e.slug = $2
  AND e.lifecycle_status = 'active'`, tenantSlug, environmentSlug).Scan(&tenantID, &environmentID, &tenantKind)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrEnvironmentNotFound
		}
		if err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
SELECT count(*)
FROM tenant_memberships
WHERE tenant_slug = $1
  AND account_id = $2
  AND revoked_at IS NULL`, tenantSlug, accountID).Scan(&membershipCnt); err != nil {
			return err
		}
		if membershipCnt == 0 {
			return fmt.Errorf("portal user membership not found")
		}

		if allowed {
			_, err = tx.Exec(ctx, `
INSERT INTO portal_environment_pat_grants (account_id, environment_id, granted_by)
VALUES ($1, $2, $3)
ON CONFLICT (account_id, environment_id) WHERE revoked_at IS NULL
DO UPDATE SET granted_by = EXCLUDED.granted_by`, accountID, environmentID, actor)
			if err != nil {
				return err
			}
			_ = insertControlEvent(ctx, tx, "portal_pat_grant_enable", tenantID, actor, map[string]any{
				"tenant_slug":      tenantSlug,
				"environment_slug": environmentSlug,
				"account_id":       accountID,
				"workspace_kind":   tenantKind,
			})
			return nil
		}

		_, err = tx.Exec(ctx, `
UPDATE portal_environment_pat_grants
SET revoked_at = now(),
    revoked_by = $3
WHERE account_id = $1
  AND environment_id = $2
  AND revoked_at IS NULL`, accountID, environmentID, actor)
		if err != nil {
			return err
		}
		_ = insertControlEvent(ctx, tx, "portal_pat_grant_disable", tenantID, actor, map[string]any{
			"tenant_slug":      tenantSlug,
			"environment_slug": environmentSlug,
			"account_id":       accountID,
			"workspace_kind":   tenantKind,
		})
		return nil
	})
}
