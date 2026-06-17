package provision

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/davesade/kilolock/internal/db"
	"github.com/davesade/kilolock/pkg/store"
)

// SharedHostConfig is input for provisioning an environment database on a
// shared Postgres instance.
type SharedHostConfig struct {
	AdminDSN string
	BaseDSN  string
	Logger   *slog.Logger
}

// SharedHostEnvironment creates the database, migrates it, syncs the tenant
// row, and returns the environment DSN.
func SharedHostEnvironment(
	ctx context.Context,
	cfg SharedHostConfig,
	tenant store.TenantRow,
	env store.EnvironmentRow,
) (dsn string, err error) {
	if cfg.AdminDSN == "" || cfg.BaseDSN == "" {
		return "", fmt.Errorf("admin and base DSN are required")
	}
	if env.DatabaseName == "" {
		return "", fmt.Errorf("environment has no database_name")
	}
	if err := CreateDatabase(ctx, cfg.AdminDSN, env.DatabaseName); err != nil {
		return "", err
	}
	dsn, err = DSNForDatabase(cfg.BaseDSN, env.DatabaseName)
	if err != nil {
		return "", err
	}
	if err := MigrateEnvironment(ctx, dsn, cfg.Logger); err != nil {
		return "", err
	}
	pool, err := db.Open(ctx, dsn)
	if err != nil {
		return "", err
	}
	defer pool.Close()
	return dsn, syncTenantOnDSN(ctx, dsn, tenant)
}

func syncTenantOnDSN(ctx context.Context, dsn string, tenant store.TenantRow) error {
	pool, err := db.Open(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	st := store.NewIsolated(pool.Pool)
	return st.EnsureTenantOnDataPlane(
		ctx,
		tenant.ID,
		tenant.Slug,
		tenant.Name,
		string(tenant.LifecycleStatus),
		tenant.BillingPlan,
		tenant.MaxEnvironments,
		tenant.MaxStateResources,
		tenant.MaxEnvironmentResources,
	)
}
