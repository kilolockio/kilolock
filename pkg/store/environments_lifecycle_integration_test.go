//go:build integration

package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/davesade/kilolock/internal/auth"
	"github.com/davesade/kilolock/internal/testdb"
)

func TestListEnvironmentsWithDSN_ExcludesInactiveLifecycle(t *testing.T) {
	s, pool := openTestStore(t)
	defer pool.Close()
	mustResetTables(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 30*time.Second)
	defer cancel()

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	activeTenant := "active-" + suffix
	suspendedTenant := "suspended-" + suffix

	if _, err := s.CreateTenant(ctx, activeTenant, "Active Tenant"); err != nil {
		t.Fatalf("create active tenant: %v", err)
	}
	if _, err := s.CreateTenant(ctx, suspendedTenant, "Suspended Tenant"); err != nil {
		t.Fatalf("create suspended tenant: %v", err)
	}
	activeEnv, err := s.CreateEnvironment(ctx, activeTenant, "prod", EnvironmentTierSharedHost, "shared")
	if err != nil {
		t.Fatalf("create active env: %v", err)
	}
	suspendedEnv, err := s.CreateEnvironment(ctx, suspendedTenant, "prod", EnvironmentTierSharedHost, "shared")
	if err != nil {
		t.Fatalf("create suspended env: %v", err)
	}
	if err := s.SetEnvironmentDSN(ctx, activeEnv.ID, "postgres://example/active"); err != nil {
		t.Fatalf("set active env dsn: %v", err)
	}
	if err := s.SetEnvironmentDSN(ctx, suspendedEnv.ID, "postgres://example/suspended"); err != nil {
		t.Fatalf("set suspended env dsn: %v", err)
	}

	if err := s.SetTenantLifecycleStatusAudit(ctx, suspendedTenant, LifecycleStatusSuspended, "itest", "policy"); err != nil {
		t.Fatalf("suspend tenant: %v", err)
	}

	rows, err := s.ListEnvironmentsWithDSN(ctx)
	if err != nil {
		t.Fatalf("list envs with dsn: %v", err)
	}
	for _, r := range rows {
		if r.TenantSlug == suspendedTenant {
			t.Fatalf("suspended tenant env leaked into routable list: %+v", r)
		}
	}
	foundActive := false
	for _, r := range rows {
		if r.TenantSlug == activeTenant && r.Slug == "prod" {
			foundActive = true
			break
		}
	}
	if !foundActive {
		t.Fatalf("expected active tenant env in list")
	}
}

func TestRequestDedicatedUpgrade_RejectsInactiveEnvironment(t *testing.T) {
	s, pool := openTestStore(t)
	defer pool.Close()
	mustResetTables(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 30*time.Second)
	defer cancel()

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	tenantSlug := "ded-life-" + suffix
	if _, err := s.CreateTenant(ctx, tenantSlug, "Dedicated Lifecycle"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	env, err := s.CreateEnvironment(ctx, tenantSlug, "prod", EnvironmentTierSharedHost, "shared")
	if err != nil {
		t.Fatalf("create env: %v", err)
	}
	if err := s.SetEnvironmentLifecycleStatusAudit(ctx, tenantSlug, env.Slug, LifecycleStatusSuspended, "itest", "policy"); err != nil {
		t.Fatalf("suspend env: %v", err)
	}
	_, err = s.RequestDedicatedUpgrade(ctx, tenantSlug, env.Slug)
	if err == nil {
		t.Fatalf("expected inactive environment rejection")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "is suspended") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestArchiveEnvironment_RenamesLabelAndFreesOriginalForReuse(t *testing.T) {
	s, pool := openTestStore(t)
	defer pool.Close()
	mustResetTables(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 30*time.Second)
	defer cancel()

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	tenantSlug := "archive-env-" + suffix
	if _, err := s.CreateTenant(ctx, tenantSlug, "Archive Env"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	env, err := s.CreateEnvironment(ctx, tenantSlug, "dev", EnvironmentTierSharedHost, "shared")
	if err != nil {
		t.Fatalf("create env: %v", err)
	}
	if err := s.SetEnvironmentLifecycleStatusAudit(ctx, tenantSlug, env.Slug, LifecycleStatusArchived, "itest", "cleanup"); err != nil {
		t.Fatalf("archive env: %v", err)
	}
	all, err := s.ListEnvironmentsAll(ctx, tenantSlug)
	if err != nil {
		t.Fatalf("list envs all: %v", err)
	}
	if len(all) == 0 {
		t.Fatalf("expected archived env to remain in metadata")
	}
	foundArchived := false
	for _, row := range all {
		if strings.HasPrefix(row.Slug, "dev--archived-") {
			foundArchived = true
			break
		}
	}
	if !foundArchived {
		t.Fatalf("expected archived environment slug with prefix %q; got %+v", "dev--archived-", all)
	}
	recreated, err := s.CreateEnvironment(ctx, tenantSlug, "dev", EnvironmentTierSharedHost, "shared")
	if err != nil {
		t.Fatalf("recreate env with original slug: %v", err)
	}
	if recreated.Slug != "dev" {
		t.Fatalf("recreated env slug=%q want dev", recreated.Slug)
	}
}

func TestDeleteEnvironmentAudit_ArchivesEnvironmentAndTokensWhenNoActiveStates(t *testing.T) {
	s, pool := openTestStore(t)
	defer pool.Close()
	mustResetTables(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 30*time.Second)
	defer cancel()

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	tenantSlug := "archive-env-delete-" + suffix
	tenant, err := s.CreateTenant(ctx, tenantSlug, "Archive Env Delete")
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	env, err := s.CreateEnvironment(ctx, tenantSlug, "dev", EnvironmentTierSharedHost, "shared")
	if err != nil {
		t.Fatalf("create env: %v", err)
	}
	stateName := tenant.WorkspaceID + "/" + env.EnvPublicID + "/example"
	stateCtx := auth.WithPrincipal(ctx, auth.Principal{
		TenantID:            tenant.ID,
		WorkspaceID:         tenant.WorkspaceID,
		TenantSlug:          tenant.Slug,
		EnvironmentID:       env.ID,
		EnvironmentPublicID: env.EnvPublicID,
		EnvironmentSlug:     env.Slug,
	})
	if err := s.WriteState(stateCtx, stateName, "", []byte(`{"version":4,"serial":1,"lineage":"11111111-2222-3333-4444-555555555555","resources":[]}`), "itest", "itest"); err != nil {
		t.Fatalf("write state: %v", err)
	}
	if err := s.SetStateLifecycleStatusAudit(stateCtx, stateName, LifecycleStatusArchived, "itest", "cleanup state"); err != nil {
		t.Fatalf("archive state: %v", err)
	}
	tok, _, err := s.CreateAPIToken(ctx, tenantSlug, env.Slug, "deploy")
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	if err := s.DeleteEnvironmentAudit(ctx, tenantSlug, env.Slug, "itest", "cleanup env"); err != nil {
		t.Fatalf("archive env via delete path: %v", err)
	}
	all, err := s.ListEnvironmentsAll(ctx, tenantSlug)
	if err != nil {
		t.Fatalf("list envs all: %v", err)
	}
	foundArchived := false
	for _, row := range all {
		if strings.HasPrefix(row.Slug, "dev--archived-") && row.LifecycleStatus == LifecycleStatusArchived {
			foundArchived = true
			break
		}
	}
	if !foundArchived {
		t.Fatalf("expected archived env slug with prefix %q; got %+v", "dev--archived-", all)
	}
	tokens, err := s.ListAPITokensAll(ctx, tenantSlug)
	if err != nil {
		t.Fatalf("list tokens: %v", err)
	}
	foundArchivedToken := false
	for _, row := range tokens {
		if row.ID == tok.ID && row.LifecycleStatus == LifecycleStatusArchived {
			foundArchivedToken = true
			break
		}
	}
	if !foundArchivedToken {
		t.Fatalf("expected token %q to be archived; got %+v", tok.ID, tokens)
	}
}

func TestDeleteEnvironmentAudit_RejectsEnvironmentWithActiveStates(t *testing.T) {
	s, pool := openTestStore(t)
	defer pool.Close()
	mustResetTables(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 30*time.Second)
	defer cancel()

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	tenantSlug := "reject-active-env-" + suffix
	tenant, err := s.CreateTenant(ctx, tenantSlug, "Reject Active Env")
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	env, err := s.CreateEnvironment(ctx, tenantSlug, "dev", EnvironmentTierSharedHost, "shared")
	if err != nil {
		t.Fatalf("create env: %v", err)
	}
	stateName := tenant.WorkspaceID + "/" + env.EnvPublicID + "/example"
	stateCtx := auth.WithPrincipal(ctx, auth.Principal{
		TenantID:            tenant.ID,
		WorkspaceID:         tenant.WorkspaceID,
		TenantSlug:          tenant.Slug,
		EnvironmentID:       env.ID,
		EnvironmentPublicID: env.EnvPublicID,
		EnvironmentSlug:     env.Slug,
	})
	if err := s.WriteState(stateCtx, stateName, "", []byte(`{"version":4,"serial":1,"lineage":"11111111-2222-3333-4444-555555555556","resources":[]}`), "itest", "itest"); err != nil {
		t.Fatalf("write state: %v", err)
	}
	err = s.DeleteEnvironmentAudit(ctx, tenantSlug, env.Slug, "itest", "cleanup env")
	if err == nil {
		t.Fatalf("expected active state rejection")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "active states") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDeleteTenantAudit_ArchivesWorkspaceWhenNoActiveEnvironments(t *testing.T) {
	s, pool := openTestStore(t)
	defer pool.Close()
	mustResetTables(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 30*time.Second)
	defer cancel()

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	tenantSlug := "archive-tenant-" + suffix
	if _, err := s.CreateTenant(ctx, tenantSlug, "Archive Tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	if err := s.SetEnvironmentLifecycleStatusAudit(ctx, tenantSlug, "default", LifecycleStatusArchived, "itest", "cleanup env"); err != nil {
		t.Fatalf("archive default env: %v", err)
	}
	if err := s.DeleteTenantAudit(ctx, tenantSlug, "itest", "cleanup workspace"); err != nil {
		t.Fatalf("archive tenant via delete path: %v", err)
	}
	tenant, err := s.GetTenantBySlug(ctx, tenantSlug)
	if err != nil {
		t.Fatalf("get tenant after archive: %v", err)
	}
	if tenant.LifecycleStatus != LifecycleStatusArchived {
		t.Fatalf("tenant lifecycle status=%q want archived", tenant.LifecycleStatus)
	}
}

func TestDeleteTenantAudit_RejectsWorkspaceWithActiveEnvironments(t *testing.T) {
	s, pool := openTestStore(t)
	defer pool.Close()
	mustResetTables(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 30*time.Second)
	defer cancel()

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	tenantSlug := "reject-active-tenant-" + suffix
	if _, err := s.CreateTenant(ctx, tenantSlug, "Reject Active Tenant"); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	err := s.DeleteTenantAudit(ctx, tenantSlug, "itest", "cleanup workspace")
	if err == nil {
		t.Fatalf("expected active environment rejection")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "active environments") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDeleteTenantAudit_RejectsPersonalWorkspace(t *testing.T) {
	s, pool := openTestStore(t)
	defer pool.Close()
	mustResetTables(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 30*time.Second)
	defer cancel()

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	email := "personal-archive-" + suffix + "@example.com"
	account, err := s.CreatePortalAccount(ctx, email, "Personal Archive", "starter", "pw")
	if err != nil {
		t.Fatalf("create portal account: %v", err)
	}
	tenant, _, err := s.CreatePersonalWorkspaceForPortalAccount(ctx, account.ID, email, email, true)
	if err != nil {
		t.Fatalf("create personal workspace: %v", err)
	}
	err = s.DeleteTenantAudit(ctx, tenant.Slug, "itest", "cleanup workspace")
	if err == nil {
		t.Fatalf("expected personal workspace rejection")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "personal workspace") {
		t.Fatalf("unexpected error: %v", err)
	}
}
