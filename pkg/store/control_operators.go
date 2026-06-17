package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const controlOperatorTenantSlug = "self-hosted"
const controlOperatorEnvSlug = "default"

type ControlOperatorTokenRow struct {
	TokenID         string
	Name            string
	TokenPrefix     string
	LifecycleStatus LifecycleStatus
	CreatedAt       time.Time
	LastUsedAt      *time.Time
	RoleKey         string
	ScopeKind       string
	ScopeRef        string
	GrantedBy       string
}

func (s *Store) CreateControlOperatorToken(ctx context.Context, name, roleKey, scopeKind, scopeRef, grantedBy string) (APITokenRow, string, error) {
	name = strings.TrimSpace(name)
	roleKey = strings.TrimSpace(roleKey)
	scopeKind = strings.TrimSpace(scopeKind)
	scopeRef = strings.TrimSpace(scopeRef)
	grantedBy = strings.TrimSpace(grantedBy)
	if name == "" || roleKey == "" {
		return APITokenRow{}, "", fmt.Errorf("name and role are required")
	}
	if scopeKind == "" {
		scopeKind = "global"
	}
	normalizedScopeRef, err := s.normalizeControlOperatorScopeRef(ctx, scopeKind, scopeRef)
	if err != nil {
		return APITokenRow{}, "", err
	}
	tokenName := name
	var row APITokenRow
	var secret string
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			tokenName = fmt.Sprintf("%s-%d", name, attempt+1)
		}
		row, secret, err = s.CreateAPIToken(ctx, controlOperatorTenantSlug, controlOperatorEnvSlug, tokenName)
		if err == nil {
			break
		}
		if !strings.Contains(err.Error(), "duplicate key value") {
			return APITokenRow{}, "", err
		}
	}
	if err != nil {
		return APITokenRow{}, "", err
	}
	if err := s.EnsurePrincipalRole(ctx, "api_token", row.ID, roleKey, scopeKind, normalizedScopeRef, grantedBy); err != nil {
		_ = s.DeleteAPITokenAudit(ctx, row.ID, grantedBy, "rollback failed operator token create")
		return APITokenRow{}, "", err
	}
	return row, secret, nil
}

func (s *Store) ListControlOperatorTokens(ctx context.Context, includeInactive bool) ([]ControlOperatorTokenRow, error) {
	q := `
SELECT tok.id::text,
       tok.name,
       tok.token_prefix,
       tok.lifecycle_status,
       tok.created_at,
       tok.last_used_at,
       COALESCE(r.key, ''),
       COALESCE(pr.scope_kind, ''),
       COALESCE(pr.scope_ref, ''),
       COALESCE(pr.granted_by, '')
FROM api_tokens tok
JOIN tenants t ON t.id = tok.tenant_id
JOIN environments e ON e.id = tok.environment_id
LEFT JOIN rbac_principal_roles pr
  ON pr.subject_kind = 'api_token'
 AND pr.subject_id = tok.id::text
 AND pr.revoked_at IS NULL
LEFT JOIN rbac_roles r ON r.id = pr.role_id
WHERE t.slug = $1
  AND e.slug = $2`
	args := []any{controlOperatorTenantSlug, controlOperatorEnvSlug}
	if !includeInactive {
		q += ` AND tok.lifecycle_status = 'active'`
	}
	q += `
ORDER BY tok.created_at DESC, tok.name ASC`
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ControlOperatorTokenRow
	for rows.Next() {
		var r ControlOperatorTokenRow
		if err := rows.Scan(
			&r.TokenID,
			&r.Name,
			&r.TokenPrefix,
			&r.LifecycleStatus,
			&r.CreatedAt,
			&r.LastUsedAt,
			&r.RoleKey,
			&r.ScopeKind,
			&r.ScopeRef,
			&r.GrantedBy,
		); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) normalizeControlOperatorScopeRef(ctx context.Context, scopeKind, scopeRef string) (string, error) {
	switch strings.TrimSpace(scopeKind) {
	case "", "global":
		return "", nil
	case "tenant":
		tenant, err := s.GetTenantBySelector(ctx, scopeRef)
		if err != nil {
			return "", err
		}
		return tenant.Slug, nil
	case "environment":
		parts := strings.SplitN(scopeRef, "/", 2)
		if len(parts) != 2 {
			return "", fmt.Errorf("environment scope must be workspace_id/environment_label")
		}
		tenant, err := s.GetTenantBySelector(ctx, parts[0])
		if err != nil {
			return "", err
		}
		return tenant.Slug + "/" + strings.TrimSpace(parts[1]), nil
	default:
		return "", fmt.Errorf("unsupported scope kind %q", scopeKind)
	}
}
