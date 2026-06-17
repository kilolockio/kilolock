package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davesade/kilolock/internal/migrate"
	"github.com/davesade/kilolock/pkg/store"
)

type migrationSummary struct {
	Total     int
	Succeeded int
	Failed    int
}

func runMigrate(args []string) int {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	allEnvs := fs.Bool("all-environments", false, "also migrate every provisioned environment database")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg := loadConfigOrExit("migrate")
	logger := newLogger(cfg.LogFormat, cfg.LogLevel)
	ctx, cancel := context.WithTimeout(cliContext(), 5*time.Minute)
	defer cancel()

	cpURL := cfg.ResolvedControlPlaneURL()
	pool := openDBURLOrExit(ctx, cpURL, logger)
	defer pool.Close()

	var err error
	if *allEnvs {
		var sum migrationSummary
		sum, err = migrateAllEnvironments(ctx, pool.Pool, logger, true)
		if err == nil {
			logger.Info("environment migrations complete", "total", sum.Total, "succeeded", sum.Succeeded, "failed", sum.Failed)
		}
	} else {
		err = migrate.Run(ctx, pool.Pool, logger)
	}
	if err != nil {
		logger.Error("migrate", "err", err)
		if msg := err.Error(); strings.Contains(msg, "\n") {
			fmt.Fprintln(os.Stderr)
			fmt.Fprintln(os.Stderr, msg)
		}
		return 1
	}
	logger.Info("migrations up to date")
	return 0
}

func migrateAllEnvironments(ctx context.Context, control *pgxpool.Pool, logger *slog.Logger, strict bool) (migrationSummary, error) {
	var summary migrationSummary
	if err := migrate.Run(ctx, control, logger); err != nil {
		return summary, fmt.Errorf("control plane: %w", err)
	}
	st := store.New(control)
	envs, err := st.ListEnvironmentsWithDSN(ctx)
	if err != nil {
		return summary, err
	}
	summary.Total = len(envs)
	const currentVersion = 17
	var firstErr error
	for _, env := range envs {
		logger.Info("migrating environment database", "tenant", env.TenantSlug, "environment", env.Slug, "database", env.DatabaseName)
		envPool, err := pgxpool.New(ctx, env.DatabaseDSN)
		if err != nil {
			msg := fmt.Sprintf("environment %s/%s connect: %v", env.TenantSlug, env.Slug, err)
			_ = st.MarkEnvironmentMigrationError(ctx, env.ID, msg)
			summary.Failed++
			if firstErr == nil {
				firstErr = errors.New(msg)
			}
			if strict {
				return summary, firstErr
			}
			logger.Error("environment migrate failed", "tenant", env.TenantSlug, "environment", env.Slug, "err", err)
			continue
		}
		if err := envPool.Ping(ctx); err != nil {
			envPool.Close()
			msg := fmt.Sprintf("environment %s/%s ping: %v", env.TenantSlug, env.Slug, err)
			_ = st.MarkEnvironmentMigrationError(ctx, env.ID, msg)
			summary.Failed++
			if firstErr == nil {
				firstErr = errors.New(msg)
			}
			if strict {
				return summary, firstErr
			}
			logger.Error("environment migrate failed", "tenant", env.TenantSlug, "environment", env.Slug, "err", err)
			continue
		}
		if err := migrate.Run(ctx, envPool, logger); err != nil {
			envPool.Close()
			msg := fmt.Sprintf("environment %s/%s migrate: %v", env.TenantSlug, env.Slug, err)
			_ = st.MarkEnvironmentMigrationError(ctx, env.ID, msg)
			summary.Failed++
			if firstErr == nil {
				firstErr = errors.New(msg)
			}
			if strict {
				return summary, firstErr
			}
			logger.Error("environment migrate failed", "tenant", env.TenantSlug, "environment", env.Slug, "err", err)
			continue
		}
		envPool.Close()
		_ = st.MarkEnvironmentMigrationSuccess(ctx, env.ID, currentVersion)
		summary.Succeeded++
	}
	if strict && firstErr != nil {
		return summary, firstErr
	}
	return summary, nil
}
