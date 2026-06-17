//go:build integration

package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/kilolockio/kilolock/pkg/testdb"
)

func TestRBACScopeAuthorizationMatrix(t *testing.T) {
	s, pool := openTestStore(t)
	defer pool.Close()
	mustResetTables(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 30*time.Second)
	defer cancel()

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	acme := "acme-" + suffix
	beta := "beta-" + suffix
	if _, err := s.CreateTenant(ctx, acme, "Acme"); err != nil {
		t.Fatalf("create tenant acme: %v", err)
	}
	if _, err := s.CreateTenant(ctx, beta, "Beta"); err != nil {
		t.Fatalf("create tenant beta: %v", err)
	}

	platformRow, platformSecret, err := s.CreateAPIToken(ctx, acme, "default", "platform")
	if err != nil {
		t.Fatalf("create platform token: %v", err)
	}
	tenantRow, tenantSecret, err := s.CreateAPIToken(ctx, acme, "default", "tenant-admin")
	if err != nil {
		t.Fatalf("create tenant-admin token: %v", err)
	}
	readOnlyRow, readOnlySecret, err := s.CreateAPIToken(ctx, acme, "default", "readonly")
	if err != nil {
		t.Fatalf("create readonly token: %v", err)
	}
	provisionerRow, provisionerSecret, err := s.CreateAPIToken(ctx, acme, "default", "provisioner")
	if err != nil {
		t.Fatalf("create provisioner token: %v", err)
	}

	// Role grants:
	// - platform_admin global
	// - tenant_admin scoped to acme
	// - support_readonly scoped to acme
	if err := s.EnsurePrincipalRole(ctx, "api_token", platformRow.ID, "platform_admin", "global", "", "itest"); err != nil {
		t.Fatalf("grant platform_admin: %v", err)
	}
	if err := s.EnsurePrincipalRole(ctx, "api_token", tenantRow.ID, "tenant_admin", "tenant", acme, "itest"); err != nil {
		t.Fatalf("grant tenant_admin: %v", err)
	}
	if err := s.EnsurePrincipalRole(ctx, "api_token", readOnlyRow.ID, "support_readonly", "tenant", acme, "itest"); err != nil {
		t.Fatalf("grant support_readonly: %v", err)
	}
	if err := s.EnsurePrincipalRole(ctx, "api_token", provisionerRow.ID, "provisioner", "tenant", acme, "itest"); err != nil {
		t.Fatalf("grant provisioner: %v", err)
	}

	platformP, ok, err := s.AuthorizeControlAPIToken(ctx, platformSecret, "tenant.create")
	if err != nil || !ok {
		t.Fatalf("platform permission tenant.create: ok=%v err=%v", ok, err)
	}
	if scoped, err := s.AuthorizeControlPrincipalScope(ctx, platformP, "tenant.create", "*", ""); err != nil || !scoped {
		t.Fatalf("platform global scope: scoped=%v err=%v", scoped, err)
	}

	tenantP, ok, err := s.AuthorizeControlAPIToken(ctx, tenantSecret, "environment.create")
	if err != nil || !ok {
		t.Fatalf("tenant-admin permission environment.create: ok=%v err=%v", ok, err)
	}
	if scoped, err := s.AuthorizeControlPrincipalScope(ctx, tenantP, "environment.create", acme, ""); err != nil || !scoped {
		t.Fatalf("tenant-admin acme scope: scoped=%v err=%v", scoped, err)
	}
	if scoped, err := s.AuthorizeControlPrincipalScope(ctx, tenantP, "environment.create", beta, ""); err != nil || scoped {
		t.Fatalf("tenant-admin beta scope: scoped=%v err=%v", scoped, err)
	}
	if scoped, err := s.AuthorizeControlPrincipalScope(ctx, tenantP, "tenant.read", "*", ""); err != nil || scoped {
		t.Fatalf("tenant-admin should not satisfy global-only scope: scoped=%v err=%v", scoped, err)
	}
	if ok, err := s.AuthorizeControlPrincipalScope(ctx, tenantP, "token.lifecycle.update", acme, "default"); err != nil || !ok {
		t.Fatalf("tenant-admin token lifecycle acme/default: ok=%v err=%v", ok, err)
	}
	if ok, err := s.AuthorizeControlPrincipalScope(ctx, tenantP, "state.delete", acme, "default"); err != nil || !ok {
		t.Fatalf("tenant-admin state delete acme/default: ok=%v err=%v", ok, err)
	}
	if ok, err := s.AuthorizeControlPrincipalScope(ctx, tenantP, "token.lifecycle.update", beta, "default"); err != nil || ok {
		t.Fatalf("tenant-admin token lifecycle beta/default should fail: ok=%v err=%v", ok, err)
	}
	if ok, err := s.AuthorizeControlPrincipalScope(ctx, tenantP, "rbac.manage", "*", ""); err != nil || ok {
		t.Fatalf("tenant-admin should not satisfy global rbac.manage scope: ok=%v err=%v", ok, err)
	}

	readonlyP, ok, err := s.AuthorizeControlAPIToken(ctx, readOnlySecret, "token.create")
	if err != nil {
		t.Fatalf("readonly authorize token.create err=%v", err)
	}
	if ok {
		t.Fatalf("readonly should not have token.create permission")
	}
	readonlyP, ok, err = s.AuthorizeControlAPIToken(ctx, readOnlySecret, "environment.read")
	if err != nil || !ok {
		t.Fatalf("readonly environment.read permission: ok=%v err=%v", ok, err)
	}
	if ok, err := s.AuthorizeControlPrincipalScope(ctx, readonlyP, "environment.read", acme, "default"); err != nil || !ok {
		t.Fatalf("readonly acme/default read scope: ok=%v err=%v", ok, err)
	}
	if ok, err := s.AuthorizeControlPrincipalScope(ctx, readonlyP, "environment.read", beta, "default"); err != nil || ok {
		t.Fatalf("readonly beta/default read scope should fail: ok=%v err=%v", ok, err)
	}
	if ok, err := s.AuthorizeControlPrincipalScope(ctx, readonlyP, "tenant.read", "*", ""); err != nil || ok {
		t.Fatalf("readonly should not satisfy global-only tenant list scope: ok=%v err=%v", ok, err)
	}

	provisionerP, ok, err := s.AuthorizeControlAPIToken(ctx, provisionerSecret, "environment.provision")
	if err != nil || !ok {
		t.Fatalf("provisioner permission environment.provision: ok=%v err=%v", ok, err)
	}
	if ok, err := s.AuthorizeControlPrincipalScope(ctx, provisionerP, "environment.provision", acme, "default"); err != nil || !ok {
		t.Fatalf("provisioner acme/default provision scope: ok=%v err=%v", ok, err)
	}
	if ok, err := s.AuthorizeControlPrincipalScope(ctx, provisionerP, "environment.provision", beta, "default"); err != nil || ok {
		t.Fatalf("provisioner beta/default provision scope should fail: ok=%v err=%v", ok, err)
	}
	if _, ok, err := s.AuthorizeControlAPIToken(ctx, provisionerSecret, "token.create"); err != nil || ok {
		t.Fatalf("provisioner should not have token.create permission: ok=%v err=%v", ok, err)
	}
}
