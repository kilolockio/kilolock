//go:build integration

package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/kilolockio/kilolock/pkg/auth"
	"github.com/kilolockio/kilolock/pkg/testdb"
)

func TestAPIToken_MultiTenantIsolation(t *testing.T) {
	s, pool := openTestStore(t)
	defer pool.Close()
	mustResetTables(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 30*time.Second)
	defer cancel()

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	slugA := "tenant-a-" + suffix
	slugB := "tenant-b-" + suffix
	if _, err := s.CreateTenant(ctx, slugA, "Tenant A"); err != nil {
		t.Fatalf("create tenant a: %v", err)
	}
	if _, err := s.CreateTenant(ctx, slugB, "Tenant B"); err != nil {
		t.Fatalf("create tenant b: %v", err)
	}

	_, secretA, err := s.CreateAPIToken(ctx, slugA, "default", "ci")
	if err != nil {
		t.Fatalf("token a: %v", err)
	}
	_, secretB, err := s.CreateAPIToken(ctx, slugB, "default", "ci")
	if err != nil {
		t.Fatalf("token b: %v", err)
	}

	pA, err := s.AuthenticateAPIToken(ctx, secretA, slugA)
	if err != nil || pA.TenantID == auth.SelfHostedTenantID {
		t.Fatalf("auth a: principal=%+v err=%v", pA, err)
	}

	// Wrong tenant slug with valid token for another tenant must fail.
	if _, err := s.AuthenticateAPIToken(ctx, secretA, slugB); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("expected unauthenticated for cross-tenant basic, got %v", err)
	}

	pB, err := s.AuthenticateAPIToken(ctx, secretB, "")
	if err != nil {
		t.Fatalf("bearer b: %v", err)
	}
	if pA.TenantID == pB.TenantID {
		t.Fatal("tenants should differ")
	}

	// Write state as tenant A; tenant B cannot see it.
	stateName := "shared-name"
	tfA := []byte(`{"version":4,"terraform_version":"1.13.4","serial":1,"lineage":"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa","outputs":{},"resources":[]}`)
	ctxA := auth.WithPrincipal(ctx, pA)
	if err := s.WriteState(ctxA, stateName, "", tfA, "test", "actor-a"); err != nil {
		t.Fatalf("write a: %v", err)
	}
	ctxB := auth.WithPrincipal(ctx, pB)
	_, err = s.GetCurrentState(ctxB, stateName)
	if !errors.Is(err, ErrStateNotFound) {
		t.Fatalf("tenant b should not see tenant a state: %v", err)
	}
}

func TestAuthenticateBackendToken_WithPortalPATMembership(t *testing.T) {
	s, pool := openTestStore(t)
	defer pool.Close()
	mustResetTables(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 30*time.Second)
	defer cancel()

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	email := "pat-" + suffix + "@example.com"

	account, err := s.CreatePortalAccount(ctx, email, "PAT Co", "starter", "pw")
	if err != nil {
		t.Fatalf("create portal account: %v", err)
	}
	tenant, _, err := s.CreateTenantForPortalAccount(ctx, account.ID, "", "PAT Tenant", email, true)
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	env, err := s.GetEnvironmentByTenantSlug(ctx, tenant.Slug, "default")
	if err != nil {
		t.Fatalf("get default env: %v", err)
	}
	_, secret, err := s.RotatePortalPersonalAccessToken(ctx, account.ID, email)
	if err != nil {
		t.Fatalf("rotate pat: %v", err)
	}
	stateName := tenant.WorkspaceID + "/" + env.EnvPublicID + "/example"

	if _, err := s.AuthenticateBackendToken(ctx, secret, tenant.WorkspaceID, stateName); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("expected unauthenticated before PAT grant, got %v", err)
	}

	if err := s.SetPortalEnvironmentPATGrant(ctx, tenant.Slug, env.Slug, account.ID, email, true); err != nil {
		t.Fatalf("grant pat access: %v", err)
	}

	p, err := s.AuthenticateBackendToken(ctx, secret, tenant.WorkspaceID, stateName)
	if err != nil {
		t.Fatalf("authenticate backend token: %v", err)
	}
	if p.Source != "portal-pat" {
		t.Fatalf("source=%q want portal-pat", p.Source)
	}
	if p.UserID != account.ID {
		t.Fatalf("user id=%q want %q", p.UserID, account.ID)
	}
	if p.WorkspaceID != tenant.WorkspaceID || p.EnvironmentPublicID != env.EnvPublicID {
		t.Fatalf("principal scope=%q/%q want %q/%q", p.WorkspaceID, p.EnvironmentPublicID, tenant.WorkspaceID, env.EnvPublicID)
	}

	if _, err := s.pool.Exec(ctx, `
UPDATE tenant_memberships
SET revoked_at = now(), updated_at = now()
WHERE tenant_slug = $1
  AND account_id = $2
  AND revoked_at IS NULL`, tenant.Slug, account.ID); err != nil {
		t.Fatalf("revoke membership: %v", err)
	}
	if _, err := s.AuthenticateBackendToken(ctx, secret, tenant.WorkspaceID, stateName); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("expected unauthenticated after membership revoke, got %v", err)
	}
}

func TestAuthenticateBackendToken_WithPersonalWorkspacePAT(t *testing.T) {
	s, pool := openTestStore(t)
	defer pool.Close()
	mustResetTables(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 30*time.Second)
	defer cancel()

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	email := "personal-pat-" + suffix + "@example.com"

	account, err := s.CreatePortalAccount(ctx, email, "Personal PAT", "starter", "pw")
	if err != nil {
		t.Fatalf("create portal account: %v", err)
	}
	tenant, _, err := s.CreatePersonalWorkspaceForPortalAccount(ctx, account.ID, email, email, true)
	if err != nil {
		t.Fatalf("create personal workspace: %v", err)
	}
	env, err := s.GetEnvironmentByTenantSlug(ctx, tenant.Slug, "default")
	if err != nil {
		t.Fatalf("get default env: %v", err)
	}
	_, secret, err := s.RotatePortalPersonalAccessToken(ctx, account.ID, email)
	if err != nil {
		t.Fatalf("rotate pat: %v", err)
	}
	stateName := tenant.WorkspaceID + "/" + env.EnvPublicID + "/example"

	p, err := s.AuthenticateBackendToken(ctx, secret, tenant.WorkspaceID, stateName)
	if err != nil {
		t.Fatalf("authenticate personal workspace pat: %v", err)
	}
	if p.Source != "portal-pat" {
		t.Fatalf("source=%q want portal-pat", p.Source)
	}
}
