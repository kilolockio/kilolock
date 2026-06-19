package store

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/kilolockio/kilolock/pkg/auth"
)

// PortalRoleAllowsControlPermission maps portal workspace roles to the subset
// of cloud-control permissions that should be reachable through a human PAT.
func PortalRoleAllowsControlPermission(role, permission string) bool {
	role = strings.ToLower(strings.TrimSpace(role))
	permission = strings.TrimSpace(permission)
	switch role {
	case "owner":
		switch permission {
		case "tenant.read",
			"tenant.lifecycle.update",
			"environment.read",
			"environment.create",
			"environment.lifecycle.update",
			"token.read",
			"token.create",
			"token.lifecycle.update",
			"state.delete",
			"state.config.update",
			"tenant.billing.checkout",
			"environment.transfer.update":
			return true
		}
	case "tenant_admin":
		switch permission {
		case "tenant.read",
			"environment.read",
			"environment.create",
			"environment.lifecycle.update",
			"token.read",
			"token.create",
			"token.lifecycle.update",
			"state.delete",
			"state.config.update":
			return true
		}
	case "billing_admin":
		switch permission {
		case "tenant.read", "tenant.billing.checkout":
			return true
		}
	}
	return false
}

// AuthorizeControlPortalPAT authenticates a human portal PAT for customer
// tenant/environment-scoped cloud-control routes.
func (s *Store) AuthorizeControlPortalPAT(ctx context.Context, secret, permission, targetTenant, targetEnv string) (auth.Principal, bool, error) {
	secret = strings.TrimSpace(secret)
	permission = strings.TrimSpace(permission)
	targetTenant = strings.TrimSpace(targetTenant)
	targetEnv = strings.TrimSpace(targetEnv)
	if secret == "" || permission == "" || targetTenant == "" || targetTenant == "*" {
		return auth.Principal{}, false, nil
	}

	hash := auth.HashAPIToken(secret)
	var (
		p    auth.Principal
		role string
	)

	if targetEnv != "" {
		err := s.pool.QueryRow(ctx, `
SELECT t.id::text, t.workspace_id, t.slug,
       e.id::text, e.env_public_id, e.slug, e.state_lock_default_mode, COALESCE(e.database_instance_key, 'shared'),
       a.id::text, a.email, tm.role, t.lifecycle_status, t.billing_plan, t.max_environments, t.max_state_resources, t.max_environment_resources
FROM portal_personal_access_tokens pat
JOIN portal_accounts a ON a.id = pat.account_id
JOIN tenant_memberships tm ON tm.account_id = a.id AND tm.revoked_at IS NULL
JOIN tenants t ON t.slug = tm.tenant_slug
JOIN environments e ON e.tenant_id = t.id
WHERE pat.token_hash = $1
  AND pat.revoked_at IS NULL
  AND t.slug = $2
  AND e.slug = $3
  AND t.lifecycle_status = 'active'
  AND e.lifecycle_status = 'active'
LIMIT 1
`, hash, targetTenant, targetEnv).Scan(
			&p.TenantID, &p.WorkspaceID, &p.TenantSlug,
			&p.EnvironmentID, &p.EnvironmentPublicID, &p.EnvironmentSlug, &p.EnvironmentStateLockDefaultMode, &p.DatabaseInstanceKey,
			&p.UserID, &p.Email, &role, &p.TenantLifecycleStatus, &p.BillingPlan, &p.MaxEnvironments, &p.MaxStateResources, &p.MaxEnvironmentResources,
		)
		if errors.Is(err, pgx.ErrNoRows) {
			return auth.Principal{}, false, nil
		}
		if err != nil {
			return auth.Principal{}, false, err
		}
	} else {
		err := s.pool.QueryRow(ctx, `
SELECT t.id::text, t.workspace_id, t.slug,
       '' AS environment_id, '' AS env_public_id, '' AS env_slug, '' AS state_lock_default_mode, 'shared' AS database_instance_key,
       a.id::text, a.email, tm.role, t.lifecycle_status, t.billing_plan, t.max_environments, t.max_state_resources, t.max_environment_resources
FROM portal_personal_access_tokens pat
JOIN portal_accounts a ON a.id = pat.account_id
JOIN tenant_memberships tm ON tm.account_id = a.id AND tm.revoked_at IS NULL
JOIN tenants t ON t.slug = tm.tenant_slug
WHERE pat.token_hash = $1
  AND pat.revoked_at IS NULL
  AND t.slug = $2
  AND t.lifecycle_status = 'active'
LIMIT 1
`, hash, targetTenant).Scan(
			&p.TenantID, &p.WorkspaceID, &p.TenantSlug,
			&p.EnvironmentID, &p.EnvironmentPublicID, &p.EnvironmentSlug, &p.EnvironmentStateLockDefaultMode, &p.DatabaseInstanceKey,
			&p.UserID, &p.Email, &role, &p.TenantLifecycleStatus, &p.BillingPlan, &p.MaxEnvironments, &p.MaxStateResources, &p.MaxEnvironmentResources,
		)
		if errors.Is(err, pgx.ErrNoRows) {
			return auth.Principal{}, false, nil
		}
		if err != nil {
			return auth.Principal{}, false, err
		}
	}

	if !PortalRoleAllowsControlPermission(role, permission) {
		return auth.Principal{}, false, nil
	}

	_, _ = s.pool.Exec(ctx, `
UPDATE portal_personal_access_tokens
SET last_used_at = now()
WHERE token_hash = $1
  AND revoked_at IS NULL`, hash)

	p.Source = "portal-pat-control"
	return p, true, nil
}
