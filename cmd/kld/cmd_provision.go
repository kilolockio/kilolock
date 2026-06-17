package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/kilolockio/kilolock/pkg/provision"
	"github.com/kilolockio/kilolock/pkg/store"
)

func runProvision(args []string) int {
	if len(args) == 0 {
		printProvisionUsage(os.Stderr)
		return 2
	}
	switch args[0] {
	case "dedicated":
		return runProvisionDedicated(args[1:])
	case "help", "--help", "-h":
		printProvisionUsage(os.Stdout)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "kld provision: unknown subcommand %q\n\n", args[0])
		printProvisionUsage(os.Stderr)
		return 2
	}
}

func printProvisionUsage(w *os.File) {
	fmt.Fprintf(w, `Usage:
  kld provision dedicated
    Process environments with tier=dedicated_host and status=provisioning.

Requires:
  KL_GCP_PROJECT_ID
  KL_DEDICATED_TF_MODULE  (path to deploy/gcp/modules/dedicated-environment)
  terraform and gcloud credentials on PATH

Optional:
  KL_GCP_REGION
  KL_TERRAFORM_STATE_DIR
  KL_DEDICATED_SQL_TIER
  KL_DEDICATED_DSN_FORM   (tcp|socket, default tcp)
`)
}

func runProvisionDedicated(args []string) int {
	fs := flag.NewFlagSet("provision dedicated", flag.ContinueOnError)
	iacVersion := fs.String("iac-version", "", "Desired IaC CLI version (used with KL_IAC_BIN).")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg := loadConfigOrExit("provision dedicated")
	logger := newLogger(cfg.LogFormat, cfg.LogLevel)
	gcpCfg := provision.LoadGCPDedicatedConfigFromEnv()
	resolvedBin, berr := resolveIACBinary(gcpCfg.IACBinary, *iacVersion, cfg.IACBinary, cfg.IACVersion)
	if berr != nil {
		fmt.Fprintf(os.Stderr, "provision dedicated: %v\n", berr)
		return 2
	}
	gcpCfg.IACBinary = resolvedBin

	ctx, cancel := context.WithTimeout(cliContext(), 30*time.Minute)
	defer cancel()

	pool := openDBURLOrExit(ctx, cfg.ResolvedControlPlaneURL(), logger)
	defer pool.Close()
	control := store.New(pool.Pool)

	n, err := provision.RunDedicatedWorker(ctx, control, gcpCfg, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "provision dedicated: %v\n", err)
		return 1
	}
	fmt.Printf("provisioned %d dedicated environment(s)\n", n)
	return 0
}
