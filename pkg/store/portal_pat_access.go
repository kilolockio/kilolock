package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type PortalEnvironmentPATAccessRow struct {
	WorkspaceID     string
	TenantSlug      string
	EnvironmentID   string
	EnvPublicID     string
	EnvironmentSlug string
	AccountID       string
	Email           string
	Company         string
	Plan            string
	Role            string
	TokenPrefix     string
	PATCreatedAt    *time.Time
	PATLastUsedAt   *time.Time
	AccessMode      string
	GrantedBy       string
	GrantCreatedAt  *time.Time
}

func (s *Store) ListPortalEnvironmentPATAccess(ctx context.Context, environmentRef string) ([]PortalEnvironmentPATAccessRow, error) {
	env, err := s.GetEnvironmentBySelector(ctx, environmentRef)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
SELECT t.workspace_id,
       t.slug,
       e.id::text,
       e.env_public_id,
       e.slug,
       a.id::text,
       a.email,
       a.company,
       a.plan,
       tm.role,
       pat.token_prefix,
       pat.created_at,
       pat.last_used_at,
       CASE
         WHEN t.kind = 'personal' AND t.personal_owner_account_id = a.id THEN 'personal_owner'
         ELSE 'grant'
       END,
       COALESCE(g.granted_by, ''),
       g.created_at
FROM environments e
JOIN tenants t ON t.id = e.tenant_id
JOIN tenant_memberships tm ON tm.tenant_slug = t.slug AND tm.revoked_at IS NULL
JOIN portal_accounts a ON a.id = tm.account_id
JOIN portal_personal_access_tokens pat ON pat.account_id = a.id AND pat.revoked_at IS NULL
LEFT JOIN portal_environment_pat_grants g
  ON g.account_id = a.id
 AND g.environment_id = e.id
 AND g.revoked_at IS NULL
WHERE e.id = $1
  AND t.lifecycle_status = 'active'
  AND e.lifecycle_status = 'active'
  AND (
        (t.kind = 'personal' AND t.personal_owner_account_id = a.id)
        OR g.id IS NOT NULL
      )
ORDER BY a.email ASC`, env.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []PortalEnvironmentPATAccessRow
	for rows.Next() {
		var row PortalEnvironmentPATAccessRow
		if err := rows.Scan(
			&row.WorkspaceID,
			&row.TenantSlug,
			&row.EnvironmentID,
			&row.EnvPublicID,
			&row.EnvironmentSlug,
			&row.AccountID,
			&row.Email,
			&row.Company,
			&row.Plan,
			&row.Role,
			&row.TokenPrefix,
			&row.PATCreatedAt,
			&row.PATLastUsedAt,
			&row.AccessMode,
			&row.GrantedBy,
			&row.GrantCreatedAt,
		); err != nil {
			return nil, err
		}
		row.GrantedBy = strings.TrimSpace(row.GrantedBy)
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if env.ID == "" {
		return nil, fmt.Errorf("environment not found")
	}
	return out, nil
}
