//go:build integration

package store

import (
	"context"
	"testing"
	"time"

	"github.com/kilolockio/kilolock/pkg/testdb"
)

func TestEnsureTenantOnDataPlane_UpdatesEntitlementsAndLifecycle(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	mustResetTables(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 10*time.Second)
	defer cancel()

	tenantID := "11111111-2222-3333-4444-555555555599"
	if err := s.EnsureTenantOnDataPlane(ctx, tenantID, "ws_demo", "Demo", "active", "starter", 1, 100, 500); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	if err := s.EnsureTenantOnDataPlane(ctx, tenantID, "ws_demo", "Demo", "suspended", "pro", 3, 10000, 50000); err != nil {
		t.Fatalf("update tenant: %v", err)
	}

	var lifecycleStatus, billingPlan string
	var maxEnvironments, maxStateResources, maxEnvironmentResources int
	if err := s.pool.QueryRow(ctx, `
SELECT lifecycle_status, billing_plan, max_environments, max_state_resources, max_environment_resources
FROM tenants
WHERE id = $1::uuid`, tenantID).Scan(&lifecycleStatus, &billingPlan, &maxEnvironments, &maxStateResources, &maxEnvironmentResources); err != nil {
		t.Fatalf("query tenant: %v", err)
	}
	if lifecycleStatus != "suspended" {
		t.Fatalf("lifecycle_status=%q want suspended", lifecycleStatus)
	}
	if billingPlan != "pro" {
		t.Fatalf("billing_plan=%q want pro", billingPlan)
	}
	if maxEnvironments != 3 || maxStateResources != 10000 || maxEnvironmentResources != 50000 {
		t.Fatalf("entitlements=(%d,%d,%d) want (3,10000,50000)", maxEnvironments, maxStateResources, maxEnvironmentResources)
	}
}
