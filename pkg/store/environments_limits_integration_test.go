//go:build integration

package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/davesade/kilolock/internal/testdb"
)

func TestCreateEnvironment_EnforcesTenantMaxEnvironments(t *testing.T) {
	s, pool := openTestStore(t)
	defer pool.Close()
	mustResetTables(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 30*time.Second)
	defer cancel()

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	tenantSlug := "limit-" + suffix
	tenantName := "Limit Tenant"
	row, err := s.CreateTenant(ctx, tenantSlug, tenantName)
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	// Force max_environments=1 (default) explicitly for clarity.
	_, err = s.pool.Exec(ctx, `UPDATE tenants SET max_environments = 1 WHERE id = $1`, row.ID)
	if err != nil {
		t.Fatalf("set max_environments: %v", err)
	}

	// Default env already exists from CreateTenant; creating another should fail.
	_, err = s.CreateEnvironment(ctx, tenantSlug, "prod", EnvironmentTierSharedHost, "shared")
	if err == nil {
		t.Fatalf("expected environment limit error")
	}
}
