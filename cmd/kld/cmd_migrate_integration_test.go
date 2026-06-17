//go:build integration

package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/kilolockio/kilolock/pkg/db"
	"github.com/kilolockio/kilolock/pkg/migrate"
	"github.com/kilolockio/kilolock/pkg/store"
	"github.com/kilolockio/kilolock/pkg/testdb"
)

func TestMigrateAllEnvironments_NonStrictMarksError(t *testing.T) {
	baseURL := testdb.DataPlaneBaseURL()
	if baseURL == "" {
		t.Skip("set KL_DATABASE_URL for integration test")
	}
	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 2*time.Minute)
	defer cancel()

	pool, err := db.Open(ctx, baseURL)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer pool.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := migrate.Run(ctx, pool.Pool, logger); err != nil {
		t.Fatalf("migrate control plane: %v", err)
	}

	st := store.New(pool.Pool)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	tenantSlug := "mig-" + suffix
	tenant, err := st.CreateTenant(ctx, tenantSlug, "Migration Tenant")
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	env, err := st.CreateEnvironment(ctx, tenant.Slug, "prod", store.EnvironmentTierSharedHost, "shared")
	if err != nil {
		t.Fatalf("create env: %v", err)
	}
	if err := st.SetEnvironmentDSN(ctx, env.ID, "postgres://invalid:65535/nope?sslmode=disable"); err != nil {
		t.Fatalf("set env dsn: %v", err)
	}

	sum, err := migrateAllEnvironments(ctx, pool.Pool, logger, false)
	if err != nil {
		t.Fatalf("migrateAllEnvironments non-strict: %v", err)
	}
	if sum.Failed < 1 {
		t.Fatalf("expected at least one failed env migration, summary=%+v", sum)
	}

	updated, err := st.GetEnvironmentByID(ctx, env.ID)
	if err != nil {
		t.Fatalf("reload env: %v", err)
	}
	if updated.LastMigrationAt == nil {
		t.Fatalf("expected last_migration_at to be set")
	}
	if updated.LastMigrationError == "" {
		t.Fatalf("expected last_migration_error to be set")
	}
}
