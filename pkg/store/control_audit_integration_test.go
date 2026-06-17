//go:build integration

package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/davesade/kilolock/internal/testdb"
)

func TestControlAudit_LifecycleEvents(t *testing.T) {
	s, pool := openTestStore(t)
	defer pool.Close()
	mustResetTables(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 30*time.Second)
	defer cancel()

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	tenant := "audit-" + suffix
	if _, err := s.CreateTenant(ctx, tenant, "Audit Tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	if _, err := s.CreateEnvironment(ctx, tenant, "prod", "shared", ""); err != nil {
		t.Fatalf("create env: %v", err)
	}
	tok, _, err := s.CreateAPIToken(ctx, tenant, "prod", "deploy")
	if err != nil {
		t.Fatalf("create token: %v", err)
	}

	if err := s.SetTenantLifecycleStatusAudit(ctx, tenant, LifecycleStatusSuspended, "ops@example.com", "billing"); err != nil {
		t.Fatalf("tenant lifecycle: %v", err)
	}
	if err := s.SetEnvironmentLifecycleStatusAudit(ctx, tenant, "prod", LifecycleStatusSuspended, "ops@example.com", "billing"); err != nil {
		t.Fatalf("environment lifecycle: %v", err)
	}
	if err := s.SetAPITokenLifecycleStatusAudit(ctx, tok.ID, LifecycleStatusSuspended, "ops@example.com", "billing"); err != nil {
		t.Fatalf("token lifecycle: %v", err)
	}

	assertEvent := func(kind string) {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `
SELECT count(*)
FROM events
WHERE kind = $1
  AND actor = 'ops@example.com'
  AND payload->>'to' = 'suspended'
`, kind).Scan(&n); err != nil {
			t.Fatalf("query event %s: %v", kind, err)
		}
		if n != 1 {
			t.Fatalf("event %s count=%d want 1", kind, n)
		}
	}
	assertEvent("tenant_lifecycle_update")
	assertEvent("environment_lifecycle_update")
	assertEvent("api_token_lifecycle_update")
}

func TestControlAudit_DeleteTenantArchivesAndEmitsLifecycleEvent(t *testing.T) {
	s, pool := openTestStore(t)
	defer pool.Close()
	mustResetTables(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 30*time.Second)
	defer cancel()

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	tenant := "audit-archive-" + suffix
	if _, err := s.CreateTenant(ctx, tenant, "Audit Archive Tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	if err := s.SetEnvironmentLifecycleStatusAudit(ctx, tenant, "default", LifecycleStatusArchived, "ops@example.com", "cleanup env"); err != nil {
		t.Fatalf("archive env: %v", err)
	}
	if err := s.DeleteTenantAudit(ctx, tenant, "ops@example.com", "cleanup workspace"); err != nil {
		t.Fatalf("archive tenant: %v", err)
	}

	var n int
	if err := pool.QueryRow(ctx, `
SELECT count(*)
FROM events
WHERE kind = 'tenant_lifecycle_update'
  AND actor = 'ops@example.com'
  AND payload->>'to' = 'archived'
  AND payload->>'reason' = 'cleanup workspace'
`).Scan(&n); err != nil {
		t.Fatalf("query archive event: %v", err)
	}
	if n != 1 {
		t.Fatalf("tenant archive event count=%d want 1", n)
	}
}

func TestControlAudit_RBACGrantRevokeEvents(t *testing.T) {
	s, pool := openTestStore(t)
	defer pool.Close()
	mustResetTables(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 30*time.Second)
	defer cancel()

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	tenant := "audit-rbac-" + suffix
	if _, err := s.CreateTenant(ctx, tenant, "RBAC Tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	tok, _, err := s.CreateAPIToken(ctx, tenant, "default", "deployer")
	if err != nil {
		t.Fatalf("create token: %v", err)
	}

	if err := s.EnsurePrincipalRole(ctx, "api_token", tok.ID, "tenant_admin", "tenant", tenant, "rbac-admin@example.com"); err != nil {
		t.Fatalf("grant role: %v", err)
	}
	// idempotent ensure should not create a second active grant or event.
	if err := s.EnsurePrincipalRole(ctx, "api_token", tok.ID, "tenant_admin", "tenant", tenant, "rbac-admin@example.com"); err != nil {
		t.Fatalf("grant role idempotent: %v", err)
	}

	var grantID string
	if err := pool.QueryRow(ctx, `
SELECT id::text
FROM rbac_principal_roles
WHERE subject_kind = 'api_token'
  AND subject_id = $1
  AND revoked_at IS NULL
ORDER BY granted_at DESC
LIMIT 1
`, tok.ID).Scan(&grantID); err != nil {
		t.Fatalf("load grant id: %v", err)
	}
	if err := s.RevokePrincipalRoleByID(ctx, grantID, "rbac-admin@example.com"); err != nil {
		t.Fatalf("revoke role: %v", err)
	}

	var createdN int
	if err := pool.QueryRow(ctx, `
SELECT count(*)
FROM events
WHERE kind = 'rbac_grant_create'
  AND actor = 'rbac-admin@example.com'
  AND payload->>'role_key' = 'tenant_admin'
`).Scan(&createdN); err != nil {
		t.Fatalf("count create events: %v", err)
	}
	if createdN != 1 {
		t.Fatalf("rbac_grant_create count=%d want 1", createdN)
	}

	var revokeN int
	if err := pool.QueryRow(ctx, `
SELECT count(*)
FROM events
WHERE kind = 'rbac_grant_revoke'
  AND actor = 'rbac-admin@example.com'
  AND payload->>'role_key' = 'tenant_admin'
`).Scan(&revokeN); err != nil {
		t.Fatalf("count revoke events: %v", err)
	}
	if revokeN != 1 {
		t.Fatalf("rbac_grant_revoke count=%d want 1", revokeN)
	}
}
