package migrate

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRun_RepairsOlderBaselineMissingEnvironmentResourceQuota(t *testing.T) {
	if os.Getenv("KL_TEST_ALLOW_SCHEMA_MUTATION") != "1" {
		t.Skip("KL_TEST_ALLOW_SCHEMA_MUTATION=1 is required for destructive schema repair test")
	}
	dsn := os.Getenv("KL_DATABASE_URL")
	if dsn == "" {
		t.Skip("KL_DATABASE_URL is required for test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer pool.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := Run(ctx, pool, logger); err != nil {
		t.Fatalf("initial migrate: %v", err)
	}

	if _, err := pool.Exec(ctx, `ALTER TABLE tenants DROP CONSTRAINT IF EXISTS tenants_max_environment_resources_positive`); err != nil {
		t.Fatalf("drop quota constraint: %v", err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE tenants DROP COLUMN IF EXISTS max_environment_resources`); err != nil {
		t.Fatalf("drop quota column: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = 18`); err != nil {
		t.Fatalf("delete migration 18 marker: %v", err)
	}

	if err := Run(ctx, pool, logger); err != nil {
		t.Fatalf("repair migrate: %v", err)
	}

	var maxEnvironments, maxEnvironmentResources int
	if err := pool.QueryRow(ctx, `SELECT max_environments, max_environment_resources FROM tenants WHERE slug = 'self-hosted'`).Scan(&maxEnvironments, &maxEnvironmentResources); err != nil {
		t.Fatalf("read repaired quotas: %v", err)
	}
	if maxEnvironments != 0 {
		t.Fatalf("max_environments=%d want 0", maxEnvironments)
	}
	if maxEnvironmentResources != 0 {
		t.Fatalf("max_environment_resources=%d want 0", maxEnvironmentResources)
	}
}
