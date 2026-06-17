//go:build integration

package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/kilolockio/kilolock/pkg/testdb"
)

func TestRequestDedicatedUpgrade(t *testing.T) {
	s, pool := openTestStore(t)
	defer pool.Close()
	mustResetTables(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 30*time.Second)
	defer cancel()

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	slug := "ded-" + suffix
	if _, err := s.CreateTenant(ctx, slug, "Ded Tenant"); err != nil {
		t.Fatalf("tenant: %v", err)
	}
	env, err := s.CreateEnvironment(ctx, slug, "prod", EnvironmentTierSharedHost, "shared")
	if err != nil {
		t.Fatalf("env: %v", err)
	}
	if err := s.SetEnvironmentDSN(ctx, env.ID, "postgres://example/old"); err != nil {
		t.Fatalf("set dsn: %v", err)
	}

	up, err := s.RequestDedicatedUpgrade(ctx, slug, "prod")
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if up.Tier != EnvironmentTierDedicatedHost {
		t.Fatalf("tier: %s", up.Tier)
	}
	if up.Status != EnvironmentStatusProvisioning {
		t.Fatalf("status: %s", up.Status)
	}
	if up.SourceDatabaseDSN != "postgres://example/old" {
		t.Fatalf("source dsn: %q", up.SourceDatabaseDSN)
	}

	pending, err := s.ListDedicatedProvisioning(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(pending) < 1 {
		t.Fatal("expected pending env")
	}
}
