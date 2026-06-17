package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/kilolockio/kilolock/pkg/auth"
	"github.com/kilolockio/kilolock/pkg/bootstrapinit"
	"github.com/kilolockio/kilolock/pkg/config"
	"github.com/kilolockio/kilolock/pkg/db"
	"github.com/kilolockio/kilolock/pkg/migrate"
	"github.com/kilolockio/kilolock/pkg/provision"
	"github.com/kilolockio/kilolock/pkg/store"
)

func main() {
	sub := "serve"
	subArgs := []string{}
	if len(os.Args) > 1 {
		sub = os.Args[1]
		subArgs = os.Args[2:]
	}
	switch sub {
	case "serve":
		os.Exit(runServeControl(subArgs))
	case "provision":
		os.Exit(runProvisionControl(subArgs))
	case "init":
		os.Exit(runInitControl(subArgs))
	case "seal-status":
		os.Exit(runSealStatusControl(subArgs))
	case "migrate":
		os.Exit(runMigrateControl(subArgs))
	case "retention":
		os.Exit(runRetentionControl(subArgs))
	case "help", "-h", "--help":
		printUsage()
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "klc: unknown subcommand %q\n\n", sub)
		printUsage()
		os.Exit(2)
	}
}

func runServeControl(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg := config.Load()
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "klc: %v\n", err)
		return 2
	}
	logger := newLogger(cfg.LogFormat, cfg.LogLevel)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cpPool, err := db.Open(ctx, cfg.ResolvedControlPlaneURL())
	if err != nil {
		logger.Error("connect control plane db", "err", err)
		return 1
	}
	defer cpPool.Close()
	control := store.New(cpPool.Pool)

	apiToken := strings.TrimSpace(os.Getenv("KL_CONTROL_TOKEN"))
	if err := validateControlServeSecurityConfig(cfg, apiToken); err != nil {
		fmt.Fprintln(os.Stderr, "klc serve:", err)
		return 2
	}
	s := newServer(control, cfg, logger, apiToken)
	addr := strings.TrimSpace(os.Getenv("KL_CONTROL_LISTEN_ADDR"))
	if addr == "" {
		addr = ":8090"
	}
	logger.Info("klc listening", "addr", addr)
	var errServe error
	if cfg.ResolvedTLSMode() == "required" {
		// Use plain net/http server in-place for TLS to keep control and
		// backend transport posture aligned.
		errServe = (&http.Server{Addr: addr, Handler: s.routes()}).ListenAndServeTLS(cfg.TLSCertFile, cfg.TLSKeyFile)
	} else {
		errServe = s.listenAndServe(addr)
	}
	if err := errServe; err != nil {
		logger.Error("server stopped", "err", err)
		return 1
	}
	return 0
}

func validateControlServeSecurityConfig(cfg config.Config, apiToken string) error {
	if cfg.ResolvedInitMode() == "prod" && strings.TrimSpace(apiToken) == "" {
		return fmt.Errorf("KL_CONTROL_TOKEN is required in prod init mode")
	}
	if cfg.ResolvedInitMode() == "prod" && cfg.ResolvedProdTLSRequired() {
		if cfg.ResolvedTLSMode() != "required" {
			return fmt.Errorf("KL_TLS_MODE=required is mandatory in prod init mode when KL_PROD_TLS_REQUIRED=true")
		}
		if strings.TrimSpace(cfg.TLSCertFile) == "" || strings.TrimSpace(cfg.TLSKeyFile) == "" {
			return fmt.Errorf("KL_TLS_CERT_FILE and KL_TLS_KEY_FILE are required when KL_TLS_MODE=required")
		}
		if !config.UsesStrictDBTLS(cfg.ResolvedControlPlaneURL()) {
			return fmt.Errorf("control-plane DSN must set sslmode=verify-full in prod mode when KL_PROD_TLS_REQUIRED=true")
		}
	}
	return nil
}

func runMigrateControl(args []string) int {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg := config.Load()
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "klc migrate: %v\n", err)
		return 2
	}
	logger := newLogger(cfg.LogFormat, cfg.LogLevel)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := db.Open(ctx, cfg.ResolvedControlPlaneURL())
	if err != nil {
		logger.Error("connect control plane db", "err", err)
		return 1
	}
	defer pool.Close()
	if err := migrate.Run(ctx, pool.Pool, logger); err != nil {
		fmt.Fprintf(os.Stderr, "klc migrate: %v\n", err)
		return 1
	}
	fmt.Println("migrations up to date")
	return 0
}

func runInitControl(args []string) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	tenant := fs.String("tenant", "operator", "Initial operator tenant slug.")
	name := fs.String("tenant-name", "Operator", "Initial operator tenant name.")
	tokenName := fs.String("token-name", "operator-bootstrap", "Initial operator token name.")
	token := fs.String("token", "", "Optional explicit token secret (generated if omitted).")
	outputFile := fs.String("output-file", "", "Optional path to write bootstrap JSON output.")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg := config.Load()
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "klc init: %v\n", err)
		return 2
	}
	logger := newLogger(cfg.LogFormat, cfg.LogLevel)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := db.Open(ctx, cfg.ResolvedControlPlaneURL())
	if err != nil {
		logger.Error("connect control plane db", "err", err)
		return 1
	}
	defer pool.Close()
	st := store.New(pool.Pool)
	status, err := st.GetSystemInitStatus(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "klc init:", err)
		return 1
	}
	if status.Initialized {
		at := "unknown"
		if status.InitializedAt != nil {
			at = status.InitializedAt.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(os.Stderr, "already initialized (mode=%s by=%s at=%s)\n", status.InitMode, status.InitializedBy, at)
		return 1
	}
	secret := strings.TrimSpace(*token)
	if secret == "" {
		generated, _, _, err := auth.NewAPIToken()
		if err != nil {
			fmt.Fprintln(os.Stderr, "klc init: generate token:", err)
			return 1
		}
		secret = generated
	}
	if err := st.BootstrapTenantToken(ctx, *tenant, *name, *tokenName, secret); err != nil {
		fmt.Fprintln(os.Stderr, "klc init:", err)
		return 1
	}
	if err := st.EnsureAPITokenRoleByName(ctx, *tenant, "default", *tokenName, "platform_admin", "control-init"); err != nil {
		fmt.Fprintln(os.Stderr, "klc init: grant platform_admin:", err)
		return 1
	}
	if err := st.MarkSystemInitialized(ctx, "prod", "control-init"); err != nil {
		fmt.Fprintln(os.Stderr, "klc init:", err)
		return 1
	}
	if path := strings.TrimSpace(*outputFile); path != "" {
		if err := bootstrapinit.WriteFile(path, bootstrapinit.Output{
			Tenant:     *tenant,
			TenantName: *name,
			TokenName:  *tokenName,
			Token:      secret,
			CreatedAt:  time.Now().UTC(),
		}); err != nil {
			fmt.Fprintf(os.Stderr, "klc init: write output file: %v\n", err)
			return 1
		}
	}
	fmt.Println("klc init complete")
	fmt.Printf("tenant=%s token_name=%s\n", *tenant, *tokenName)
	fmt.Printf("token=%s\n", secret)
	fmt.Println("store this token in a secret manager; it is shown only now.")
	return 0
}

func runSealStatusControl(args []string) int {
	fs := flag.NewFlagSet("seal-status", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg := config.Load()
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "klc seal-status: %v\n", err)
		return 2
	}
	logger := newLogger(cfg.LogFormat, cfg.LogLevel)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := db.Open(ctx, cfg.ResolvedControlPlaneURL())
	if err != nil {
		logger.Error("connect control plane db", "err", err)
		return 1
	}
	defer pool.Close()
	st := store.New(pool.Pool)
	status, err := st.GetSystemInitStatus(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "klc seal-status:", err)
		return 1
	}
	if !status.Initialized {
		fmt.Println("sealed=true initialized=false")
		return 0
	}
	at := "unknown"
	if status.InitializedAt != nil {
		at = status.InitializedAt.UTC().Format(time.RFC3339)
	}
	fmt.Println("sealed=false initialized=true")
	fmt.Printf("mode=%s initialized_by=%s initialized_at=%s\n", status.InitMode, status.InitializedBy, at)
	return 0
}

func runProvisionControl(args []string) int {
	if len(args) == 0 {
		printProvisionUsageControl()
		return 2
	}
	switch args[0] {
	case "run":
		return runProvisionRunControl(args[1:])
	case "help", "-h", "--help":
		printProvisionUsageControl()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "klc provision: unknown subcommand %q\n\n", args[0])
		printProvisionUsageControl()
		return 2
	}
}

func runProvisionRunControl(args []string) int {
	fs := flag.NewFlagSet("provision run", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg := config.Load()
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "klc provision run: %v\n", err)
		return 2
	}
	logger := newLogger(cfg.LogFormat, cfg.LogLevel)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	pool, err := db.Open(ctx, cfg.ResolvedControlPlaneURL())
	if err != nil {
		logger.Error("connect control plane db", "err", err)
		return 1
	}
	defer pool.Close()
	control := store.New(pool.Pool)

	gcpCfg := provision.LoadGCPDedicatedConfigFromEnv()
	if strings.TrimSpace(gcpCfg.IACBinary) == "" {
		gcpCfg.IACBinary = cfg.IACBinary
	}
	n, err := provision.RunDedicatedWorker(ctx, control, gcpCfg, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "klc provision run: %v\n", err)
		return 1
	}
	fmt.Printf("provisioned %d dedicated environment(s)\n", n)
	return 0
}

func printProvisionUsageControl() {
	fmt.Print(`klc provision

Usage:
  klc provision run

Runs dedicated-host provisioning worker for environments in:
  tier=dedicated_host, status=provisioning

Required environment:
  KL_GCP_PROJECT_ID
  KL_DEDICATED_TF_MODULE

Optional environment:
  KL_GCP_REGION
  KL_TERRAFORM_STATE_DIR
  KL_DEDICATED_SQL_TIER
  KL_DEDICATED_DSN_FORM
  KL_IAC_BIN
`)
}

func printUsage() {
	fmt.Print(`klc

Usage:
  klc serve
  klc provision run
  klc migrate
  klc retention purge [--older-than 720h] [--tenant acme] [--apply]
  klc init [--tenant operator --tenant-name "Operator" --token-name operator-bootstrap] [--output-file /path/init.json]
  klc seal-status
`)
}

func runRetentionControl(args []string) int {
	if len(args) == 0 {
		printRetentionUsageControl()
		return 2
	}
	switch args[0] {
	case "purge":
		return runRetentionPurgeControl(args[1:])
	case "help", "-h", "--help":
		printRetentionUsageControl()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "klc retention: unknown subcommand %q\n\n", args[0])
		printRetentionUsageControl()
		return 2
	}
}

func runRetentionPurgeControl(args []string) int {
	fs := flag.NewFlagSet("retention purge", flag.ContinueOnError)
	olderThan := fs.Duration("older-than", 30*24*time.Hour, "Purge archived tenants older than this duration.")
	tenant := fs.String("tenant", "", "Optional single tenant slug to target.")
	actor := fs.String("actor", "control-cli", "Actor recorded in retention purge audit log.")
	reason := fs.String("reason", "", "Reason for purge operation (required with --apply).")
	applyMode := fs.Bool("apply", false, "Execute deletions. Without this flag, command runs in dry-run mode.")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg := config.Load()
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "klc retention purge: %v\n", err)
		return 2
	}
	logger := newLogger(cfg.LogFormat, cfg.LogLevel)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := db.Open(ctx, cfg.ResolvedControlPlaneURL())
	if err != nil {
		logger.Error("connect control plane db", "err", err)
		return 1
	}
	defer pool.Close()
	st := store.New(pool.Pool)
	if err := validateRetentionPurgeApply(*applyMode, *reason); err != nil {
		fmt.Fprintln(os.Stderr, "klc retention purge:", err)
		return 2
	}

	cutoff := time.Now().UTC().Add(-*olderThan)
	candidates, err := st.ListArchivedTenantPurgeCandidates(ctx, cutoff, *tenant)
	if err != nil {
		fmt.Fprintln(os.Stderr, "klc retention purge:", err)
		return 1
	}
	fmt.Printf("retention purge candidates (cutoff=%s, count=%d)\n", cutoff.Format(time.RFC3339), len(candidates))
	for _, c := range candidates {
		fmt.Printf("- %s archived_at=%s envs=%d tokens=%d (active=%d revoked=%d)\n",
			c.TenantSlug,
			c.LifecycleChangedAt.UTC().Format(time.RFC3339),
			c.EnvironmentCount,
			c.APITokenCount,
			c.ActiveAPITokenCount,
			c.RevokedAPITokenCount,
		)
	}
	if !*applyMode {
		for _, c := range candidates {
			_ = st.RecordRetentionPurgeAudit(ctx, store.RetentionPurgeAuditEvent{
				TenantSlug: c.TenantSlug,
				CutoffAt:   cutoff,
				Actor:      *actor,
				Reason:     *reason,
				ApplyMode:  false,
				Status:     "dry_run",
			})
		}
		fmt.Println("dry-run: no rows deleted (re-run with --apply to execute purge)")
		return 0
	}
	for _, c := range candidates {
		res, err := st.PurgeArchivedTenant(ctx, c.TenantSlug, cutoff)
		if err != nil {
			_ = st.RecordRetentionPurgeAudit(ctx, store.RetentionPurgeAuditEvent{
				TenantSlug:   c.TenantSlug,
				CutoffAt:     cutoff,
				Actor:        *actor,
				Reason:       *reason,
				ApplyMode:    true,
				Status:       "failed",
				ErrorMessage: err.Error(),
			})
			fmt.Fprintf(os.Stderr, "purge %s: %v\n", c.TenantSlug, err)
			return 1
		}
		_ = st.RecordRetentionPurgeAudit(ctx, store.RetentionPurgeAuditEvent{
			TenantSlug:          c.TenantSlug,
			CutoffAt:            cutoff,
			Actor:               *actor,
			Reason:              *reason,
			ApplyMode:           true,
			Status:              "applied",
			DeletedTenants:      res.DeletedTenants,
			DeletedEnvironments: res.DeletedEnvironments,
			DeletedAPITokens:    res.DeletedAPITokens,
		})
		fmt.Printf("purged %s: tenants=%d envs=%d tokens=%d\n",
			res.TenantSlug, res.DeletedTenants, res.DeletedEnvironments, res.DeletedAPITokens)
	}
	fmt.Println("retention purge complete")
	return 0
}

func printRetentionUsageControl() {
	fmt.Print(`klc retention

Usage:
  klc retention purge [--older-than 720h] [--tenant acme] [--apply --reason "policy-expired"]

Workflow:
  1) Run without --apply to preview archived-tenant purge candidates.
  2) Re-run with --apply and --reason to delete matching tenants and dependent metadata.
`)
}

func newLogger(format, level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	if format == "json" {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}
