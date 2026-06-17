package provision

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/davesade/kilolock/pkg/store"
)

// RunDedicatedWorker processes all environments in dedicated_host/provisioning state.
func RunDedicatedWorker(ctx context.Context, control *store.Store, cfg GCPDedicatedConfig, logger *slog.Logger) (int, error) {
	if logger == nil {
		logger = slog.Default()
	}
	envs, err := control.ListDedicatedProvisioning(ctx)
	if err != nil {
		return 0, err
	}
	var done int
	for _, env := range envs {
		tenant, err := control.GetTenantBySlug(ctx, env.TenantSlug)
		if err != nil {
			_ = control.FailDedicatedProvision(ctx, env.ID, err.Error())
			continue
		}
		logger.Info("provisioning dedicated host",
			"tenant", env.TenantSlug,
			"environment", env.Slug,
			"database", env.DatabaseName,
		)
		conn, dsn, err := ProvisionDedicatedHost(ctx, cfg, env, tenant, logger)
		if err != nil {
			_ = control.FailDedicatedProvision(ctx, env.ID, err.Error())
			logger.Error("dedicated provision failed",
				"tenant", env.TenantSlug,
				"environment", env.Slug,
				"err", err,
			)
			continue
		}
		if err := control.CompleteDedicatedProvision(ctx, env.ID, conn, dsn); err != nil {
			return done, fmt.Errorf("complete provision %s/%s: %w", env.TenantSlug, env.Slug, err)
		}
		logger.Info("dedicated host ready",
			"tenant", env.TenantSlug,
			"environment", env.Slug,
			"connection_name", conn,
		)
		done++
	}
	return done, nil
}
