package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/kilolockio/kilolock/pkg/auth"
)

type RBACGrantRow struct {
	ID          string
	SubjectKind string
	SubjectID   string
	RoleKey     string
	ScopeKind   string
	ScopeRef    string
	GrantedBy   string
	GrantedAt   string
	RevokedAt   string
}

func (s *Store) EnsurePrincipalRole(ctx context.Context, subjectKind, subjectID, roleKey, scopeKind, scopeRef, grantedBy string) error {
	subjectKind = strings.TrimSpace(subjectKind)
	subjectID = strings.TrimSpace(subjectID)
	roleKey = strings.TrimSpace(roleKey)
	scopeKind = strings.TrimSpace(scopeKind)
	scopeRef = strings.TrimSpace(scopeRef)
	grantedBy = strings.TrimSpace(grantedBy)
	if subjectKind == "" || subjectID == "" || roleKey == "" {
		return fmt.Errorf("subject kind/id and role key are required")
	}
	if scopeKind == "" {
		scopeKind = "global"
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var grantID string
		err := tx.QueryRow(ctx, `
		INSERT INTO rbac_principal_roles (subject_kind, subject_id, role_id, scope_kind, scope_ref, granted_by)
		SELECT $1, $2, r.id, $4, $5, $6
		FROM rbac_roles r
		WHERE r.key = $3
		ON CONFLICT (subject_kind, subject_id, role_id, scope_kind, scope_ref)
			WHERE revoked_at IS NULL
			DO NOTHING
		RETURNING id::text
	`, subjectKind, subjectID, roleKey, scopeKind, scopeRef, grantedBy).Scan(&grantID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}

		var tenantID string
		if subjectKind == "api_token" {
			_ = tx.QueryRow(ctx, `SELECT tenant_id::text FROM api_tokens WHERE id = $1`, subjectID).Scan(&tenantID)
		}
		payload, err := json.Marshal(map[string]any{
			"grant_id":     grantID,
			"subject_kind": subjectKind,
			"subject_id":   subjectID,
			"role_key":     roleKey,
			"scope_kind":   scopeKind,
			"scope_ref":    scopeRef,
			"granted_by":   grantedBy,
		})
		if err != nil {
			return err
		}
		return insertControlEvent(ctx, tx, "rbac_grant_create", tenantID, grantedBy, payload)
	})
}

func (s *Store) ListRBACGrants(ctx context.Context, includeRevoked bool) ([]RBACGrantRow, error) {
	q := `
SELECT pr.id::text, pr.subject_kind, pr.subject_id, r.key,
       pr.scope_kind, pr.scope_ref, pr.granted_by,
       pr.granted_at::text, COALESCE(pr.revoked_at::text, '')
FROM rbac_principal_roles pr
JOIN rbac_roles r ON r.id = pr.role_id`
	if !includeRevoked {
		q += ` WHERE pr.revoked_at IS NULL`
	}
	q += ` ORDER BY pr.granted_at DESC`
	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RBACGrantRow
	for rows.Next() {
		var r RBACGrantRow
		if err := rows.Scan(&r.ID, &r.SubjectKind, &r.SubjectID, &r.RoleKey, &r.ScopeKind, &r.ScopeRef, &r.GrantedBy, &r.GrantedAt, &r.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) RevokePrincipalRoleByID(ctx context.Context, grantID, revokedBy string) error {
	grantID = strings.TrimSpace(grantID)
	revokedBy = strings.TrimSpace(revokedBy)
	if grantID == "" {
		return fmt.Errorf("grant id is required")
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var (
			subjectKind string
			subjectID   string
			scopeKind   string
			scopeRef    string
			roleKey     string
		)
		err := tx.QueryRow(ctx, `
WITH revoked AS (
	UPDATE rbac_principal_roles pr
	SET revoked_at = now(),
	    granted_by = CASE WHEN pr.granted_by = '' AND $2 <> '' THEN $2 ELSE pr.granted_by END
	WHERE pr.id = $1
	  AND pr.revoked_at IS NULL
	RETURNING pr.subject_kind, pr.subject_id, pr.scope_kind, pr.scope_ref, pr.role_id
)
SELECT r.subject_kind, r.subject_id, r.scope_kind, r.scope_ref, rr.key
FROM revoked r
JOIN rbac_roles rr ON rr.id = r.role_id
`, grantID, revokedBy).Scan(&subjectKind, &subjectID, &scopeKind, &scopeRef, &roleKey)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("grant not found or already revoked")
			}
			return err
		}

		var tenantID string
		if subjectKind == "api_token" {
			_ = tx.QueryRow(ctx, `SELECT tenant_id::text FROM api_tokens WHERE id = $1`, subjectID).Scan(&tenantID)
		}
		payload, err := json.Marshal(map[string]any{
			"grant_id":     grantID,
			"subject_kind": subjectKind,
			"subject_id":   subjectID,
			"role_key":     roleKey,
			"scope_kind":   scopeKind,
			"scope_ref":    scopeRef,
			"revoked_by":   revokedBy,
		})
		if err != nil {
			return err
		}
		return insertControlEvent(ctx, tx, "rbac_grant_revoke", tenantID, revokedBy, payload)
	})
}

func (s *Store) EnsureAPITokenRoleByName(ctx context.Context, tenantSlug, envSlug, tokenName, roleKey, grantedBy string) error {
	tenantSlug = strings.TrimSpace(tenantSlug)
	envSlug = strings.TrimSpace(envSlug)
	tokenName = strings.TrimSpace(tokenName)
	if envSlug == "" {
		envSlug = "default"
	}
	if tenantSlug == "" || tokenName == "" {
		return fmt.Errorf("tenant and token name are required")
	}
	var tokenID string
	err := s.pool.QueryRow(ctx, `
		SELECT tok.id::text
		FROM api_tokens tok
		JOIN tenants t ON t.id = tok.tenant_id
		JOIN environments e ON e.id = tok.environment_id
		WHERE t.slug = $1 AND e.slug = $2 AND tok.name = $3
	`, tenantSlug, envSlug, tokenName).Scan(&tokenID)
	if err != nil {
		return err
	}
	return s.EnsurePrincipalRole(ctx, "api_token", tokenID, roleKey, "global", "", grantedBy)
}

func (s *Store) AuthorizeControlAPIToken(ctx context.Context, secret, permission string) (auth.Principal, bool, error) {
	secret = strings.TrimSpace(secret)
	permission = strings.TrimSpace(permission)
	if secret == "" || permission == "" {
		return auth.Principal{}, false, nil
	}
	hash := auth.HashAPIToken(secret)
	var p auth.Principal
	var tokenID string
	err := s.pool.QueryRow(ctx, `
		SELECT t.id::text, t.workspace_id, t.slug, e.id::text, e.env_public_id, e.slug, COALESCE(e.database_instance_key,'shared'), tok.name, tok.id::text
		FROM api_tokens tok
		JOIN tenants t ON t.id = tok.tenant_id
		JOIN environments e ON e.id = tok.environment_id
		WHERE tok.token_hash = $1
		  AND tok.revoked_at IS NULL
		  AND tok.lifecycle_status = 'active'
		  AND e.lifecycle_status = 'active'
	`, hash).Scan(&p.TenantID, &p.WorkspaceID, &p.TenantSlug, &p.EnvironmentID, &p.EnvironmentPublicID, &p.EnvironmentSlug, &p.DatabaseInstanceKey, &p.Email, &tokenID)
	if err != nil {
		return auth.Principal{}, false, nil
	}
	p.Source = "control-api-token"
	p.UserID = tokenID

	var n int
	err = s.pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM rbac_principal_roles pr
		JOIN rbac_role_permissions rp ON rp.role_id = pr.role_id
		JOIN rbac_permissions perm ON perm.id = rp.permission_id
		WHERE pr.subject_kind = 'api_token'
		  AND pr.subject_id = $1
		  AND pr.revoked_at IS NULL
		  AND perm.key = $2
		  AND (
		        pr.scope_kind = 'global'
		     OR (pr.scope_kind = 'tenant' AND pr.scope_ref = $3)
		     OR (pr.scope_kind = 'environment' AND pr.scope_ref = ($3 || '/' || $4))
		  )
	`, tokenID, permission, p.TenantSlug, p.EnvironmentSlug).Scan(&n)
	if err != nil {
		return auth.Principal{}, false, err
	}
	return p, n > 0, nil
}

// AuthorizeControlPrincipalScope validates that principal p holds the given
// permission at the requested scope target.
//
// targetTenant:
//   - "*" means global-only (tenant/environment scopes do not satisfy).
//   - "<tenant>" means tenant scope for that tenant (or global).
//
// targetEnv:
//   - empty means tenant/global route.
//   - non-empty means environment/global route for tenant/env.
func (s *Store) AuthorizeControlPrincipalScope(ctx context.Context, p auth.Principal, permission, targetTenant, targetEnv string) (bool, error) {
	permission = strings.TrimSpace(permission)
	targetTenant = strings.TrimSpace(targetTenant)
	targetEnv = strings.TrimSpace(targetEnv)
	if strings.TrimSpace(p.UserID) == "" || permission == "" {
		return false, nil
	}
	var (
		q    string
		args []any
	)
	switch {
	case targetTenant == "*":
		q = `
SELECT COUNT(*)
FROM rbac_principal_roles pr
JOIN rbac_role_permissions rp ON rp.role_id = pr.role_id
JOIN rbac_permissions perm ON perm.id = rp.permission_id
WHERE pr.subject_kind = 'api_token'
  AND pr.subject_id = $1
  AND pr.revoked_at IS NULL
  AND perm.key = $2
  AND pr.scope_kind = 'global'`
		args = []any{p.UserID, permission}
	case targetTenant != "" && targetEnv != "":
		q = `
SELECT COUNT(*)
FROM rbac_principal_roles pr
JOIN rbac_role_permissions rp ON rp.role_id = pr.role_id
JOIN rbac_permissions perm ON perm.id = rp.permission_id
WHERE pr.subject_kind = 'api_token'
  AND pr.subject_id = $1
  AND pr.revoked_at IS NULL
  AND perm.key = $2
  AND (
        pr.scope_kind = 'global'
     OR (pr.scope_kind = 'tenant' AND pr.scope_ref = $3)
     OR (pr.scope_kind = 'environment' AND pr.scope_ref = ($3 || '/' || $4))
  )`
		args = []any{p.UserID, permission, targetTenant, targetEnv}
	case targetTenant != "":
		q = `
SELECT COUNT(*)
FROM rbac_principal_roles pr
JOIN rbac_role_permissions rp ON rp.role_id = pr.role_id
JOIN rbac_permissions perm ON perm.id = rp.permission_id
WHERE pr.subject_kind = 'api_token'
  AND pr.subject_id = $1
  AND pr.revoked_at IS NULL
  AND perm.key = $2
  AND (
        pr.scope_kind = 'global'
     OR (pr.scope_kind = 'tenant' AND pr.scope_ref = $3)
  )`
		args = []any{p.UserID, permission, targetTenant}
	default:
		return false, nil
	}
	var n int
	if err := s.pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}
