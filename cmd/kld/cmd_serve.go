package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davesade/kilolock/internal/backend"
	"github.com/davesade/kilolock/internal/config"
	"github.com/davesade/kilolock/internal/migrate"
	"github.com/davesade/kilolock/internal/routing"
	"github.com/davesade/kilolock/pkg/store"
)

func runServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	listen := fs.String("listen", "", "override KL_LISTEN_ADDR")
	skipMigrate := fs.Bool("skip-migrate", false, "skip auto-applying migrations on startup")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg := loadConfigOrExit("serve")
	if *listen != "" {
		cfg.ListenAddr = *listen
	}
	if err := validateServeSecurityConfig(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "kld serve:", err)
		return 2
	}
	logger := newLogger(cfg.LogFormat, cfg.LogLevel)

	startCtx, startCancel := context.WithTimeout(cliContext(), defaultTimeout)
	defer startCancel()

	cpURL := cfg.ResolvedControlPlaneURL()
	dataURL := cfg.ResolvedDataPlaneURL()

	cpPool := openDBURLOrExit(startCtx, cpURL, logger)
	defer cpPool.Close()

	dataPool := cpPool
	if dataURL != cpURL {
		dataPool = openDBURLOrExit(startCtx, dataURL, logger)
		defer dataPool.Close()
	}

	if !*skipMigrate {
		if err := migrate.Run(startCtx, cpPool.Pool, logger); err != nil {
			logger.Error("migrate control plane", "err", err)
			return 1
		}
		if dataURL != cpURL {
			if err := migrate.Run(startCtx, dataPool.Pool, logger); err != nil {
				logger.Error("migrate default data plane", "err", err)
				return 1
			}
		}
	}

	controlStore := store.New(cpPool.Pool)
	if err := validateEnvironmentRoutingConfig(startCtx, controlStore, cfg); err != nil {
		logger.Error("environment routing config invalid", "err", err)
		return 1
	}
	switch cfg.ResolvedInitMode() {
	case "prod":
		if cfg.BootstrapTenantSlug != "" || cfg.BootstrapTokenSecret != "" {
			logger.Error("bootstrap env vars are disabled in prod init mode; run `kl operator init` instead")
			return 1
		}
		initSt, err := controlStore.GetSystemInitStatus(startCtx)
		if err != nil {
			logger.Error("read system init status", "err", err)
			return 1
		}
		if !initSt.Initialized {
			logger.Error("metadata backend is not initialized; run `kl operator init`")
			return 1
		}
	default:
		bootstrapped, err := bootstrapAuthIfConfigured(startCtx, controlStore, cfg, logger)
		if err != nil {
			logger.Error("auth bootstrap", "err", err)
			return 1
		}
		if bootstrapped {
			if err := controlStore.MarkSystemInitialized(startCtx, "dev", "serve-bootstrap"); err != nil {
				logger.Error("mark system initialized", "err", err)
				return 1
			}
		}
	}

	authn, err := buildAuthenticator(cfg, controlStore)
	if err != nil {
		logger.Error("auth setup", "err", err)
		return 1
	}

	cache := routing.NewPoolCache(cfg.MaxEnvironmentPools).
		WithCircuitBreaker(cfg.RoutingCircuitFailureThreshold, time.Duration(cfg.RoutingCircuitCooldownSeconds)*time.Second)
	perInstanceMax := make(map[string]int32, len(cfg.DataPlaneInstanceMaxConns))
	for k, v := range cfg.DataPlaneInstanceMaxConns {
		perInstanceMax[k] = int32(v)
	}
	perInstanceMaxPools := make(map[string]int, len(cfg.DataPlaneInstanceMaxPools))
	for k, v := range cfg.DataPlaneInstanceMaxPools {
		perInstanceMaxPools[k] = v
	}
	logger.Info("routing pool connection caps", "default_max_conns", cfg.DataPlaneDefaultMaxConns, "instance_overrides", cfg.DataPlaneInstanceMaxConns)
	logger.Info("routing pool count caps", "default_max_pools", cfg.DataPlaneDefaultMaxPools, "instance_overrides", cfg.DataPlaneInstanceMaxPools)
	router := routing.NewRouter(dataPool.Pool, controlStore, cache).
		WithInstanceMaxConns(int32(cfg.DataPlaneDefaultMaxConns), perInstanceMax).
		WithInstanceMaxPools(cfg.DataPlaneDefaultMaxPools, perInstanceMaxPools)
	defer router.Close()

	defaultStore := store.New(dataPool.Pool)
	handler := backend.New(defaultStore, logger).
		WithStoreResolver(router.StoreFor).
		WithRoutingStatsProvider(func() map[string]any {
			st := cache.Stats()
			return map[string]any{
				"routing_cache_open_pools": st.OpenPools,
				"routing_cache_hits":       st.Hits,
				"routing_cache_misses":     st.Misses,
				"routing_cache_opens":      st.Opens,
				"routing_cache_evicts":     st.Evicts,
				"routing_instances":        st.Instances,
			}
		}).
		WithAuthenticator(authn).
		Handler()
	switch cfg.ResolvedAuthMode() {
	case "open":
		logger.Warn("HTTP authentication disabled (open mode) — not for production")
	case "static":
		logger.Info("HTTP authentication: static shared token (single-tenant)")
	case "database":
		logger.Info("HTTP authentication: per-tenant API tokens (database)")
	}

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 15 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if cfg.RoutingStatsIntervalSeconds > 0 {
		go logRoutingStats(ctx, logger, cache, time.Duration(cfg.RoutingStatsIntervalSeconds)*time.Second)
	}
	if !*skipMigrate && cfg.EnvMigrationEnabled {
		go migrateEnvironmentsLoop(ctx, cpPool.Pool, logger, time.Duration(cfg.EnvMigrationIntervalSeconds)*time.Second)
	}

	errs := make(chan error, 1)
	go func() {
		logger.Info("kld serve listening", "addr", cfg.ListenAddr, "version", version)
		if cfg.ResolvedTLSMode() == "required" {
			errs <- srv.ListenAndServeTLS(cfg.TLSCertFile, cfg.TLSKeyFile)
			return
		}
		errs <- srv.ListenAndServe()
	}()

	select {
	case err := <-errs:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server crashed", "err", err)
			return 1
		}
	case <-ctx.Done():
		logger.Info("shutdown signal received")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Error("graceful shutdown failed", "err", err)
			return 1
		}
	}
	return 0
}

func validateServeSecurityConfig(cfg config.Config) error {
	if cfg.ResolvedInitMode() == "prod" && cfg.ResolvedAuthMode() == "open" {
		return fmt.Errorf("KL_AUTH_MODE=open is not allowed in prod init mode")
	}
	if cfg.ResolvedInitMode() == "prod" && cfg.ResolvedProdTLSRequired() {
		if cfg.ResolvedTLSMode() != "required" {
			return fmt.Errorf("KL_TLS_MODE=required is mandatory in prod init mode when KL_PROD_TLS_REQUIRED=true")
		}
		if strings.TrimSpace(cfg.TLSCertFile) == "" || strings.TrimSpace(cfg.TLSKeyFile) == "" {
			return fmt.Errorf("KL_TLS_CERT_FILE and KL_TLS_KEY_FILE are required when KL_TLS_MODE=required")
		}
		checks := map[string]string{
			"control-plane":      cfg.ResolvedControlPlaneURL(),
			"default-data-plane": cfg.ResolvedDataPlaneURL(),
		}
		for k, v := range cfg.DataPlaneInstanceURLs {
			checks["data-plane-instance:"+k] = v
		}
		for k, v := range cfg.DataPlaneInstanceAdminURLs {
			checks["data-plane-admin:"+k] = v
		}
		if admin := strings.TrimSpace(cfg.DataPlaneAdminURL); admin != "" {
			checks["default-data-plane-admin"] = admin
		}
		for label, dsn := range checks {
			if !config.UsesStrictDBTLS(dsn) {
				return fmt.Errorf("%s DSN must set sslmode=verify-full in prod mode when KL_PROD_TLS_REQUIRED=true", label)
			}
		}
	}
	return nil
}

func migrateEnvironmentsLoop(ctx context.Context, control *pgxpool.Pool, logger *slog.Logger, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	run := func() {
		mctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		sum, err := migrateAllEnvironments(mctx, control, logger, false)
		if err != nil {
			logger.Error("background environment migrate", "err", err)
		}
		logger.Info("background environment migrate finished", "total", sum.Total, "succeeded", sum.Succeeded, "failed", sum.Failed)
	}
	run()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run()
		}
	}
}

func logRoutingStats(ctx context.Context, logger *slog.Logger, cache *routing.PoolCache, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			st := cache.Stats()
			logger.Info("routing cache stats", "open_pools", st.OpenPools, "hits", st.Hits, "misses", st.Misses, "opens", st.Opens, "evicts", st.Evicts, "instances", st.Instances)
		}
	}
}

type environmentLister interface {
	ListAllEnvironments(ctx context.Context) ([]store.EnvironmentRow, error)
}

func validateEnvironmentRoutingConfig(ctx context.Context, st environmentLister, cfg config.Config) error {
	envs, err := st.ListAllEnvironments(ctx)
	if err != nil {
		return err
	}
	var problems []string
	for _, env := range envs {
		key := strings.TrimSpace(env.DatabaseInstanceKey)
		if key == "" {
			key = "shared"
		}
		if key == "shared" {
			continue
		}
		if strings.TrimSpace(cfg.DataPlaneInstanceURLs[key]) == "" {
			problems = append(problems, fmt.Sprintf("%s/%s missing KL_DATA_PLANE_DSN_%s", env.TenantSlug, env.Slug, strings.ToUpper(strings.ReplaceAll(key, "-", "_"))))
		}
		if strings.TrimSpace(cfg.DataPlaneInstanceAdminURLs[key]) == "" {
			problems = append(problems, fmt.Sprintf("%s/%s missing KL_DATA_PLANE_ADMIN_DSN_%s", env.TenantSlug, env.Slug, strings.ToUpper(strings.ReplaceAll(key, "-", "_"))))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("routing config errors: %s", strings.Join(problems, "; "))
}
