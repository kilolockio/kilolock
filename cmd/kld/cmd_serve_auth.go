package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/davesade/kilolock/internal/auth"
	"github.com/davesade/kilolock/internal/config"
	"github.com/davesade/kilolock/internal/provision"
	"github.com/davesade/kilolock/pkg/store"
)

func buildAuthenticator(cfg config.Config, st *store.Store) (auth.Authenticator, error) {
	switch cfg.ResolvedAuthMode() {
	case "open":
		return auth.SingleTenantAuthenticator{}, nil
	case "static":
		return auth.AuthenticatorForToken(cfg.AuthToken), nil
	case "database":
		return auth.NewTokenAuthenticator(st.AuthenticateBackendToken), nil
	default:
		return nil, fmt.Errorf("unknown auth mode %q", cfg.AuthMode)
	}
}

func bootstrapAuthIfConfigured(ctx context.Context, st *store.Store, cfg config.Config, logger *slog.Logger) (bool, error) {
	if cfg.ResolvedAuthMode() != "database" {
		return false, nil
	}
	slug := cfg.BootstrapTenantSlug
	secret := cfg.BootstrapTokenSecret
	if slug == "" || secret == "" {
		return false, nil
	}
	name := cfg.BootstrapTenantName
	if name == "" {
		name = slug
	}
	tokenName := cfg.BootstrapTokenName
	if tokenName == "" {
		tokenName = "bootstrap"
	}
	if err := st.BootstrapTenantToken(ctx, slug, name, tokenName, secret); err != nil {
		return false, err
	}
	logger.Info("bootstrapped tenant API token", "tenant_slug", slug, "token_name", tokenName)
	tenant, err := st.GetTenantBySlug(ctx, slug)
	if err != nil {
		return false, err
	}
	env, err := st.GetEnvironmentByTenantSlug(ctx, slug, "default")
	if err != nil {
		return false, err
	}
	if env.DatabaseDSN != "" {
		return true, nil
	}
	baseDSN := cfg.ResolvedDataPlaneURLForInstance(env.DatabaseInstanceKey)
	adminDSN := cfg.ResolvedDataPlaneAdminURLForInstance(env.DatabaseInstanceKey)
	if strings.TrimSpace(adminDSN) == "" {
		return true, nil
	}
	dsn, err := provision.SharedHostEnvironment(ctx, provision.SharedHostConfig{
		AdminDSN: adminDSN,
		BaseDSN:  baseDSN,
		Logger:   logger,
	}, tenant, env)
	if err != nil {
		return false, fmt.Errorf("bootstrap provision default environment: %w", err)
	}
	if err := st.SetEnvironmentDSN(ctx, env.ID, dsn); err != nil {
		return false, err
	}
	logger.Info("bootstrapped default environment database", "tenant_slug", slug, "database", env.DatabaseName)
	return true, nil
}
