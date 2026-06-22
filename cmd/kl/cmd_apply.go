package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/kilolockio/kilolock/internal/apply"
	"github.com/kilolockio/kilolock/internal/plan"
	"github.com/kilolockio/kilolock/internal/slice"
	"github.com/kilolockio/kilolock/internal/workdir"
	"github.com/kilolockio/kilolock/pkg/config"
	"github.com/kilolockio/kilolock/pkg/store"
)

// runApply is the entrypoint for `kl apply` — Kilolock's
// apply frontend for both backend-scoped and orchestrated execution.
// Every mode starts from a plan spec: either loaded from --plan-spec
// or built implicitly from the current working tree before execution.
//
// Exit codes:
//
//	0 - apply committed; new state version written
//	1 - apply failed (reservation conflict, terraform error,
//	    merge violation, commit error, etc.); see stderr
//	2 - argv / usage error
//
// The flow is intentionally CLI-thin: the heavy lifting lives in
// internal/apply. This function does argv parsing, environment
// loading, reservation/spec wiring, and output rendering.
func runApply(args []string) int {
	// Subcommands.
	if len(args) > 0 {
		switch args[0] {
		case "abort":
			return runApplyAbort(args[1:])
		}
	}

	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var (
		planSpecPath = fs.String("plan-spec", "", "Path to a kl plan spec file (output of `kl plan`). Defaults to ./kl-plan.json.")
		stateName    = fs.String("state", "", "Trunk state name to apply against. Defaults to spec.state_name (discovered from the http backend at plan time).")
		workDirFlag  = fs.String("work-dir", "", "Operator's Terraform configuration directory. Defaults to the dir containing --plan-spec, or the spec's recorded config_dir if that exists.")
		actorFlag    = fs.String("actor", "", "Actor string for the audit log (default: derived from $USER).")
		terraformBin = fs.String("terraform-bin", "", "Path to the terraform binary. Defaults to `terraform` on $PATH.")
		iacVersion   = fs.String("iac-version", "", "Desired IaC CLI version (used with --terraform-bin / KL_IAC_BIN).")
		noRefresh    = fs.Bool("no-refresh", false, "When used with --file, pass -refresh=false to terraform plan.")
		orchestrated = fs.Bool("orchestrated", false, "Use DB-backed reservation/commit orchestrator (legacy advanced mode).")
		leaseSecs    = fs.Int("lease-seconds", int(apply.DefaultLease.Seconds()), "Reservation lease duration. Long applies rely on lease renewal; increase only if your environment blocks heartbeats or you need a longer safety window.")
		skipCleanup  = fs.Bool("keep-tmp-dir", false, "Don't delete the apply temp directory after the run. Useful for debugging.")
		noColor      = fs.Bool("no-color", false, "Disable ANSI color in terraform output.")
		waitTimeout  = fs.Duration("wait-timeout", 5*time.Minute, "How long to wait for conflicting reservations to clear before failing. 0 disables the wait (fail-fast on first conflict, suitable for CI).")
		dryRun       = fs.Bool("dry-run", false, "Print apply preflight summary but do not run terraform apply.")
		confirmScope = fs.Bool("confirm-scope", false, "Acknowledge the derived scope and allow mutating scoped applies (--file/--target).")
		allowUnsafe  = fs.Bool("allow-unsafe-target", false, "Acknowledge targeted apply risk and allow `apply --target ...` execution (not required with --dry-run).")
		allowDestr   = fs.Bool("allow-destructive-scoped", false, "Allow file-scoped applies that include delete/replace/forget actions (not required with --dry-run).")
		strictTarget = fs.Bool("strict-target-preflight", false, "Fail targeted apply when preflight risk warnings are present.")
		strictFile   = fs.Bool("strict-file-preflight", false, "Fail file-scoped apply when preflight risk warnings are present.")
		strictCoex   = fs.Bool("strict-coexistence", false, "Fail if vanilla Terraform whole-state locks are active on the state instead of only warning.")
	)
	files := &fileFlag{}
	registerFlagValueAlias(fs, files, "file", "f", "Scope apply to resources declared in this file (repeatable).")
	targets := &targetFlag{}
	fs.Var(targets, "target", "Targeted apply address (repeatable).")

	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "kl apply:", err)
		fmt.Fprint(os.Stderr, applyUsage)
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "kl apply: unexpected extra arguments: %v\n", fs.Args())
		fmt.Fprint(os.Stderr, applyUsage)
		return 2
	}

	if len(files.values) > 0 && *planSpecPath != "" {
		fmt.Fprintln(os.Stderr, "kl apply: use either --file or --plan-spec, not both")
		return 2
	}
	if len(targets.values) > 0 && len(files.values) > 0 {
		fmt.Fprintln(os.Stderr, "kl apply: use either --file or --target, not both")
		return 2
	}
	if len(targets.values) > 0 && *planSpecPath != "" {
		fmt.Fprintln(os.Stderr, "kl apply: use either --target or --plan-spec, not both")
		return 2
	}

	// Defaulting plan-spec to ./kl-plan.json mirrors the
	// engineer flow: `cd module/; kl plan && kl
	// apply` writes the spec to the project root and reads it back
	// from there with no flags. We still let the operator point at
	// a different file when running CI/CD pipelines that stash the
	// spec elsewhere.
	resolvedSpecPath := *planSpecPath
	var (
		spec *plan.PlanSpec
		err  error
	)
	if len(targets.values) > 0 {
		if err := requireUnsafeTargetConfirmation(targets.values, *allowUnsafe, *dryRun); err != nil {
			fmt.Fprintln(os.Stderr, "kl apply:", err)
			return 2
		}
		spec, err = buildTargetedSpecForApply(cliContext(), *workDirFlag, resolvedSpecPath, targets.values, *terraformBin, *iacVersion)
		if err != nil {
			fmt.Fprintln(os.Stderr, "kl apply:", err)
			return 1
		}
		if err := targetGuardViolation(spec, targets.values); err != nil {
			fmt.Fprintln(os.Stderr, "kl apply:", err)
			return 1
		}
		warnings := targetedPreflightWarnings(spec, targets.values)
		renderApplyPreflight(os.Stderr, spec, "targeted", targets.values, *dryRun, warnings)
		if err := enforceTargetPreflightStrict(*strictTarget, warnings); err != nil {
			fmt.Fprintln(os.Stderr, "kl apply:", err)
			return 1
		}
		if err := requireScopedApplyConfirmation(spec, "targeted", targets.values, *confirmScope, *dryRun); err != nil {
			fmt.Fprintln(os.Stderr, "kl apply:", err)
			return 2
		}
		if *dryRun {
			fmt.Fprintln(os.Stdout, "apply dry-run complete (no terraform apply executed)")
			return 0
		}
		if !*orchestrated {
			if err := runBackendScopedApply(cliContext(), spec, resolvedSpecPath, *workDirFlag, *terraformBin, *iacVersion, *noColor); err != nil {
				fmt.Fprintln(os.Stderr, "kl apply:", err)
				return 1
			}
			fmt.Fprintf(os.Stdout, "targeted apply succeeded (state=%s, targets=%d)\n", spec.StateName, len(spec.WriteSet))
			return 0
		}
	} else if len(files.values) > 0 {
		spec, err = buildScopedSpecForApply(cliContext(), *workDirFlag, resolvedSpecPath, files.values, *terraformBin, *iacVersion, *noRefresh)
		if err != nil {
			fmt.Fprintln(os.Stderr, "kl apply:", err)
			return 1
		}
		warnings := fileScopedPreflightWarnings(spec, files.values, *noRefresh)
		renderApplyPreflight(os.Stderr, spec, "file-scoped", files.values, *dryRun, warnings)
		if err := requireDestructiveScopedConfirmation(spec, files.values, *allowDestr, *dryRun); err != nil {
			fmt.Fprintln(os.Stderr, "kl apply:", err)
			return 2
		}
		if err := enforceFilePreflightStrict(*strictFile, warnings); err != nil {
			fmt.Fprintln(os.Stderr, "kl apply:", err)
			return 1
		}
		if err := requireScopedApplyConfirmation(spec, "file-scoped", files.values, *confirmScope, *dryRun); err != nil {
			fmt.Fprintln(os.Stderr, "kl apply:", err)
			return 2
		}
		if *dryRun {
			fmt.Fprintln(os.Stdout, "apply dry-run complete (no terraform apply executed)")
			return 0
		}
		if !*orchestrated {
			if err := runBackendScopedApply(cliContext(), spec, resolvedSpecPath, *workDirFlag, *terraformBin, *iacVersion, *noColor); err != nil {
				fmt.Fprintln(os.Stderr, "kl apply:", err)
				return 1
			}
			fmt.Fprintf(os.Stdout, "scoped apply succeeded (state=%s, targets=%d)\n", spec.StateName, len(spec.WriteSet))
			return 0
		}
	} else {
		if resolvedSpecPath != "" {
			specBytes, rerr := os.ReadFile(resolvedSpecPath)
			if rerr != nil {
				fmt.Fprintf(os.Stderr, "kl apply: read --plan-spec=%s: %v\n", resolvedSpecPath, rerr)
				return 1
			}
			spec, err = plan.UnmarshalSpec(specBytes)
			if err != nil {
				fmt.Fprintf(os.Stderr, "kl apply: parse plan spec: %v\n", err)
				return 1
			}
		} else {
			workDir := *workDirFlag
			if workDir == "" {
				workDir = "."
			}
			if !hasTerraformConfigFiles(workDir) {
				fmt.Fprintf(os.Stderr,
					"kl apply: no --plan-spec provided, %s not found, and no Terraform configuration files were found in %s (run `kl plan` first or pass --work-dir)\n",
					defaultPlanSpecPath(),
					workDir,
				)
				return 2
			}
			spec, err = buildFullSpecForApply(cliContext(), *workDirFlag, *terraformBin, *iacVersion)
			if err != nil {
				fmt.Fprintln(os.Stderr, "kl apply:", err)
				return 1
			}
			renderApplyPreflight(os.Stderr, spec, "full", nil, *dryRun, nil)
			if *dryRun {
				fmt.Fprintln(os.Stdout, "apply dry-run complete (no terraform apply executed)")
				return 0
			}
		}
	}
	// When no explicit --state is provided, prefer the currently
	// discovered backend state name over the recorded spec.state_name.
	// This keeps old plan specs usable after backend state-name shape
	// fixes (for example hierarchical ws_/env_/name paths) and avoids
	// stale-spec footguns after directory/backend changes.
	resolvedStateName := firstNonEmpty(*stateName, discoverApplyStateName(resolvedSpecPath, *workDirFlag, spec))
	if resolvedStateName == "" {
		fmt.Fprintln(os.Stderr,
			"kl apply: --state is required (spec.state_name not set; was the plan run outside a terraform-init'ed directory?)")
		fmt.Fprint(os.Stderr, applyUsage)
		return 2
	}
	if len(spec.WriteSet) == 0 {
		if specHasMutatingActions(spec) {
			fmt.Fprintln(os.Stderr, "kl apply: plan spec has no write_set, but the plan summary still contains mutating actions. This usually means the spec was generated by an older build. Re-run `kl plan` and try again.")
			return 1
		}
		fmt.Fprintln(os.Stdout, "apply skipped: plan contains no writes")
		return 0
	}
	if err := enforceApplyQuotaPreflight(cliContext(), resolvedStateName, spec, resolvedSpecPath, *workDirFlag); err != nil {
		fmt.Fprintln(os.Stderr, "kl apply:", err)
		return 1
	}

	// Default apply mode is backend-driven terraform apply, so local CLI usage
	// does not require direct DB access. Every apply mode now starts from a
	// plan spec, whether loaded explicitly or built implicitly.
	if !*orchestrated {
		if resolvedSpecPath != "" {
			renderApplyPreflight(os.Stderr, spec, "spec", nil, *dryRun, nil)
			if *dryRun {
				fmt.Fprintln(os.Stdout, "apply dry-run complete (no terraform apply executed)")
				return 0
			}
		}
		if err := runBackendScopedApply(cliContext(), spec, resolvedSpecPath, *workDirFlag, *terraformBin, *iacVersion, *noColor); err != nil {
			fmt.Fprintln(os.Stderr, "kl apply:", err)
			return 1
		}
		fmt.Fprintf(os.Stdout, "apply succeeded (state=%s, targets=%d)\n", spec.StateName, len(spec.WriteSet))
		return 0
	}

	workDir, err := resolveApplyWorkDir(*workDirFlag, resolvedSpecPath, spec)
	if err != nil {
		fmt.Fprintf(os.Stderr, "kl apply: %v\n", err)
		return 2
	}

	icfg := config.Load()
	logger := newLogger(icfg.LogFormat, icfg.LogLevel)
	ctx, cancel := context.WithTimeout(cliContext(), applyTimeout)
	defer cancel()
	client, err := newAPIClientFromBackend(workDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "kl apply: discover backend API client: %v\n", err)
		return 1
	}
	st := newRemoteApplyClient(client, resolvedStateName)
	resolvedBin, berr := resolveIACBinary(*terraformBin, *iacVersion, icfg.IACBinary, icfg.IACVersion)
	if berr != nil {
		fmt.Fprintln(os.Stderr, "kl apply:", berr)
		return 2
	}

	// Coexistence note: v2 `kl apply` bypasses the v1 HTTP backend's
	// whole-state lock. If a vanilla terraform operation is currently holding
	// a lock, running v2 apply concurrently is almost certainly a mistake.
	// We warn (not fail) because stale locks are a known failure mode and v2
	// must not be bricked by them.
	if status, serr := getRemoteStateStatus(ctx, client, resolvedStateName); serr == nil && status != nil {
		locks := status.Locks
		if len(locks) == 0 && status.Lock != nil {
			locks = append(locks, *status.Lock)
		}
		if err := enforceCoexistenceStrict(*strictCoex || status.CoexistenceMode == store.StateCoexistenceStrict, resolvedStateName, locks); err != nil {
			fmt.Fprintln(os.Stderr, "kl apply:", err)
			return 1
		}
		if len(locks) > 0 {
			first := locks[0]
			if len(locks) == 1 {
				fmt.Fprintf(os.Stderr, "kl apply: warning: state %q has a v1 whole-state lock held by vanilla terraform (lock_id=%s who=%s). v2 apply bypasses this lock; avoid running concurrently.\n",
					resolvedStateName, first.LockID, first.Who)
			} else {
				fmt.Fprintf(os.Stderr, "kl apply: warning: state %q currently has %d v1 whole-state locks held by vanilla terraform (first lock_id=%s who=%s). v2 apply bypasses these locks; mixed-mode concurrency should be treated as advanced-only.\n",
					resolvedStateName, len(locks), first.LockID, first.Who)
			}
		}
		if status.ExclusiveLocks {
			fmt.Fprintf(os.Stderr, "kl apply: note: state %q is configured for exclusive whole-state locks for vanilla terraform (exclusive_locks=true). Mixed terraform/v2 apply workflows should be treated as serialized unless you switch to optimistic mode.\n",
				resolvedStateName)
		}
		if status.CoexistenceMode == store.StateCoexistenceStrict {
			fmt.Fprintf(os.Stderr, "kl apply: note: state %q enforces strict coexistence policy; vanilla terraform whole-state locks will reject v2 apply.\n",
				resolvedStateName)
		}
	}

	opts := apply.Options{
		Spec:                    spec,
		StateName:               resolvedStateName,
		Actor:                   firstNonEmpty(*actorFlag, cliActor()),
		WorkDir:                 workDir,
		TerraformBin:            resolvedBin,
		Lease:                   time.Duration(*leaseSecs) * time.Second,
		SkipCleanup:             *skipCleanup,
		NoColor:                 *noColor,
		WaitForReservations:     *waitTimeout,
		ReservationWaitNotifier: newReservationWaitRenderer(os.Stderr, resolvedStateName),
	}

	res, runErr := apply.Run(ctx, st, opts, logger)
	if res == nil {
		fmt.Fprintln(os.Stderr, "kl apply:", runErr)
		return 1
	}
	renderApplyResult(os.Stdout, res, runErr)
	if runErr != nil {
		return 1
	}
	return 0
}

func specHasMutatingActions(spec *plan.PlanSpec) bool {
	if spec == nil {
		return false
	}
	s := spec.PlanSummary
	return s.Create > 0 || s.Update > 0 || s.Delete > 0 || s.Replace > 0 || s.Forget > 0
}

func runBackendScopedApply(ctx context.Context, spec *plan.PlanSpec, specPath, workDirFlag, terraformBin, iacVersion string, noColor bool) error {
	if spec == nil {
		return fmt.Errorf("missing plan spec")
	}
	if strings.TrimSpace(specPath) == "" {
		specPath = defaultPlanSpecPath()
	}
	absDir, err := resolveApplyWorkDir(workDirFlag, specPath, spec)
	if err != nil {
		return fmt.Errorf("resolve work dir: %w", err)
	}
	icfg := config.Load()
	resolvedBin, err := resolveIACBinary(terraformBin, iacVersion, icfg.IACBinary, icfg.IACVersion)
	if err != nil {
		return err
	}
	args := []string{"apply", "-auto-approve", "-input=false", "-refresh=false"}
	if noColor {
		args = append(args, "-no-color")
	}
	for _, t := range spec.WriteSet {
		args = append(args, "-target="+t)
	}
	args = append(args, plan.TerraformVarArgs(spec.Variables)...)
	cmd := exec.CommandContext(ctx, resolvedBin, args...)
	cmd.Dir = absDir
	cmd.Env = os.Environ()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("terraform apply (%s): %w", resolvedBin, err)
	}
	return nil
}

func buildScopedSpecForApply(ctx context.Context, workDirFlag, specPath string, fileArgs []string, terraformBin, iacVersion string, noRefresh bool) (*plan.PlanSpec, error) {
	workDir := workDirFlag
	if workDir == "" {
		workDir = "."
	}
	absConfigDir, err := filepath.Abs(workDir)
	if err != nil {
		return nil, fmt.Errorf("resolve --work-dir: %w", err)
	}
	if !dirExists(absConfigDir) {
		return nil, fmt.Errorf("--work-dir=%s: not a directory", absConfigDir)
	}
	backend, err := plan.DiscoverBackend(absConfigDir)
	if err != nil {
		return nil, fmt.Errorf("--file requires terraform-init'ed http backend in %s: %w", absConfigDir, err)
	}
	raw, err := plan.FetchCurrentStateFromBackend(ctx, backend)
	if err != nil {
		return nil, fmt.Errorf("read trunk state from backend: %w", err)
	}
	scope, err := plan.NormalizeFileScope(absConfigDir, fileArgs)
	if err != nil {
		return nil, err
	}
	icfg := config.Load()
	resolvedBin, err := resolveIACBinary(terraformBin, iacVersion, icfg.IACBinary, icfg.IACVersion)
	if err != nil {
		return nil, err
	}
	fmt.Fprintln(os.Stderr, "kl apply: building scoped plan…")
	showJSON, err := plan.RunScopedTerraformPlan(ctx, resolvedBin, absConfigDir, raw, scope, plan.ScopedPlanOptions{
		Refresh: !noRefresh,
		Vars:    map[string]json.RawMessage{},
	})
	if err != nil {
		return nil, err
	}
	parsed, err := plan.ParseShowJSONBytes(showJSON)
	if err != nil {
		return nil, fmt.Errorf("parse scoped plan JSON: %w", err)
	}
	_ = plan.UpdateOwnershipCache(absConfigDir, parsed)
	spec := plan.BuildSpec(parsed, plan.SpecBuildInput{
		ConfigDir:   absConfigDir,
		GeneratedAt: time.Now().UTC(),
		StateName:   backend.StateName,
		PinAllVars:  true,
	})
	spec, err = plan.ApplyFileScope(parsed, spec, scope)
	if err != nil {
		return nil, err
	}
	if len(spec.WriteSet) == 0 {
		return nil, formatFileScopeEmptyWriteSet(parsed, spec, scope)
	}
	if specPath != "" {
		if b, merr := plan.MarshalSpec(spec); merr == nil {
			_ = os.WriteFile(specPath, b, 0o644)
		}
	}
	return spec, nil
}

func buildTargetedSpecForApply(ctx context.Context, workDirFlag, specPath string, targets []string, terraformBin, iacVersion string) (*plan.PlanSpec, error) {
	workDir := workDirFlag
	if workDir == "" {
		workDir = "."
	}
	absConfigDir, err := filepath.Abs(workDir)
	if err != nil {
		return nil, fmt.Errorf("resolve --work-dir: %w", err)
	}
	if !dirExists(absConfigDir) {
		return nil, fmt.Errorf("--work-dir=%s: not a directory", absConfigDir)
	}
	backend, err := plan.DiscoverBackend(absConfigDir)
	if err != nil {
		return nil, fmt.Errorf("--target requires terraform-init'ed http backend in %s: %w", absConfigDir, err)
	}
	raw, err := plan.FetchCurrentStateFromBackend(ctx, backend)
	if err != nil {
		return nil, fmt.Errorf("read trunk state from backend: %w", err)
	}
	icfg := config.Load()
	resolvedBin, err := resolveIACBinary(terraformBin, iacVersion, icfg.IACBinary, icfg.IACVersion)
	if err != nil {
		return nil, err
	}
	fmt.Fprintln(os.Stderr, "kl apply: building targeted plan…")
	showJSON, err := plan.RunTargetScopedTerraformPlan(ctx, resolvedBin, absConfigDir, raw, targets, plan.ScopedPlanOptions{
		Refresh: false,
		Vars:    map[string]json.RawMessage{},
	})
	if err != nil {
		return nil, err
	}
	parsed, err := plan.ParseShowJSONBytes(showJSON)
	if err != nil {
		return nil, fmt.Errorf("parse targeted plan JSON: %w", err)
	}
	_ = plan.UpdateOwnershipCache(absConfigDir, parsed)
	spec := plan.BuildSpec(parsed, plan.SpecBuildInput{
		ConfigDir:   absConfigDir,
		GeneratedAt: time.Now().UTC(),
		StateName:   backend.StateName,
		PinAllVars:  true,
	})
	allowed, aerr := plan.ExpandTargetSliceAddresses(absConfigDir, targets)
	if aerr != nil {
		return nil, aerr
	}
	if verr := plan.ValidateTargetedWriteSet(spec.WriteSet, allowed); verr != nil {
		return nil, formatTargetScopeViolation(verr, targets, spec.WriteSet)
	}
	if len(spec.WriteSet) == 0 {
		return nil, fmt.Errorf("target selection produced an empty write_set; selected targets did not own any planned writes")
	}
	if specPath != "" {
		if b, merr := plan.MarshalSpec(spec); merr == nil {
			_ = os.WriteFile(specPath, b, 0o644)
		}
	}
	return spec, nil
}

func buildFullSpecForApply(ctx context.Context, workDirFlag, terraformBin, iacVersion string) (*plan.PlanSpec, error) {
	workDir := workDirFlag
	if workDir == "" {
		workDir = "."
	}
	absConfigDir, err := filepath.Abs(workDir)
	if err != nil {
		return nil, fmt.Errorf("resolve --work-dir: %w", err)
	}
	if !dirExists(absConfigDir) {
		return nil, fmt.Errorf("--work-dir=%s: not a directory", absConfigDir)
	}

	var (
		stateName string
		srcSerial *int64
	)
	if backend, berr := plan.DiscoverBackend(absConfigDir); berr == nil {
		stateName = backend.StateName
		fmt.Fprintf(os.Stderr, "kl apply: state %q discovered from backend (%s)\n", stateName, backend.Address)
		if raw, rerr := plan.FetchCurrentStateFromBackend(ctx, backend); rerr == nil {
			if ts, perr := slice.ParseTrunkState(raw); perr == nil && ts.Serial > 0 {
				v := ts.Serial
				srcSerial = &v
			}
		} else {
			fmt.Fprintf(os.Stderr, "kl apply: warning: failed to fetch trunk state for source_serial pinning: %v\n", rerr)
		}
	} else if errors.Is(berr, plan.ErrUnsupportedBackend) {
		fmt.Fprintf(os.Stderr, "kl apply: %v (apply will require --state=…)\n", berr)
	}

	icfg := config.Load()
	resolvedBin, err := resolveIACBinary(terraformBin, iacVersion, icfg.IACBinary, icfg.IACVersion)
	if err != nil {
		return nil, err
	}
	fmt.Fprintln(os.Stderr, "kl apply: building full plan…")
	scratchRoot, err := workdir.ResolveScratchRoot(absConfigDir)
	if err != nil {
		return nil, fmt.Errorf("resolve scratch workdir: %w", err)
	}
	tfplanPath, err := os.CreateTemp(scratchRoot, ".kl-apply-*.tfplan")
	if err != nil {
		return nil, fmt.Errorf("create tmp plan file: %w", err)
	}
	tfplanPath.Close()
	defer os.Remove(tfplanPath.Name())

	if err := plan.RunTerraformPlan(ctx, resolvedBin, absConfigDir, tfplanPath.Name(), plan.DefaultPlanRunOptions()); err != nil {
		return nil, err
	}
	showJSON, err := plan.RunTerraformShow(ctx, resolvedBin, absConfigDir, tfplanPath.Name())
	if err != nil {
		return nil, err
	}
	parsed, err := plan.ParseShowJSONBytes(showJSON)
	if err != nil {
		return nil, fmt.Errorf("parse full plan JSON: %w", err)
	}
	_ = plan.UpdateOwnershipCache(absConfigDir, parsed)
	spec := plan.BuildSpec(parsed, plan.SpecBuildInput{
		ConfigDir:    absConfigDir,
		GeneratedAt:  time.Now().UTC(),
		StateName:    stateName,
		SourceSerial: srcSerial,
		PinAllVars:   true,
	})
	return spec, nil
}

const applyUsage = `Usage:
  kl apply [flags]

Builds or loads a kl plan spec, then applies its write_set
against the trunk state.

Flags:
  --plan-spec=PATH      Path to a kl plan spec file produced
                        by ` + "`kl plan`" + `. When omitted,
                        apply builds a fresh spec implicitly.
  --file=PATH, -f PATH  Build a scoped spec from selected file(s) and
                        apply it immediately (repeatable). Convenience
                        shortcut for the ADR-0014 flow.
  --target=ADDR         Build a targeted spec from selected address(es)
                        and apply it immediately (repeatable).
  --allow-unsafe-target Required to execute ` + "`apply --target`" + ` (except
                        with ` + "`--dry-run`" + `);
                        explicit acknowledgement that target-scoped
                        apply can be incomplete if target selection
                        omits required graph branches.
  --allow-destructive-scoped
                        Required to execute ` + "`apply --file`" + ` when the
                        scoped plan contains delete/replace/forget actions
                        (except with ` + "`--dry-run`" + `).
  --confirm-scope       Required to execute mutating scoped applies
                        (` + "`--file`" + ` / ` + "`--target`" + `) (except with ` + "`--dry-run`" + `).
  --strict-target-preflight
                        Fail targeted apply if preflight emits risk
                        warnings (fanout/selector spread).
  --strict-file-preflight
                        Fail file-scoped apply if preflight emits risk
                        warnings (fanout/selector spread).
  --strict-coexistence
                        Fail if vanilla Terraform whole-state locks are
                        active on the state instead of only warning.
  --dry-run             Print apply preflight summary and exit without
                        running terraform apply.
  --state=NAME          Trunk state name. Defaults to spec.state_name
                        which is auto-discovered from the http
                        backend at plan time.
  --work-dir=DIR        Operator's Terraform configuration directory.
                        Defaults to the directory containing the spec
                        file, or the spec's recorded config_dir.
  Scratch workspace/temp files:
                        default to the system temp dir for orchestrated
                        apply and to the config dir for full-plan temp
                        files, honor TF_DATA_DIR, and let KL_DATA_DIR
                        override it.
  --actor=NAME          Actor string for the audit log.
  --terraform-bin=PATH  Override the terraform binary path.
  --no-refresh          With --file: pass -refresh=false during scoped plan.
  --orchestrated        Use DB-backed reservation + row-level commit
                        orchestrator path (requires direct DB access).
  --lease-seconds=N     Reservation lease duration (default 900 = 15m).
  --keep-tmp-dir        Don't delete the apply temp directory.
  --no-color            Disable ANSI color in terraform output.
  --wait-timeout=D      How long to wait for conflicting reservations
                        to clear before failing. Default 5m. Set to 0
                        for fail-fast (CI). Wait progress streams to
                        stderr in real time.

Exit status:
  0  committed — new state version written, write_set rows applied
     (or dry-run preflight succeeded)
  1  failed    — reservation conflict, terraform error, merge violation,
                 commit failure, etc.
  2  argv      — usage error

Target guard (CI):
  Set ` + "`KL_TARGET_MAX_WRITES`" + ` and/or
  ` + "`KL_TARGET_MAX_RESERVATIONS`" + ` to fail targeted apply when
  fanout exceeds your pipeline safety limits.
`

func requireUnsafeTargetConfirmation(targets []string, allow, dryRun bool) error {
	if len(targets) == 0 || allow || dryRun {
		return nil
	}
	return fmt.Errorf(
		"--target apply blocked by safety gate (targets=%d): rerun with --allow-unsafe-target to execute, or use --dry-run for preflight only",
		len(targets),
	)
}

func requireDestructiveScopedConfirmation(spec *plan.PlanSpec, files []string, allow, dryRun bool) error {
	if spec == nil || allow || dryRun {
		return nil
	}
	destructive := spec.PlanSummary.Delete + spec.PlanSummary.Replace + spec.PlanSummary.Forget
	if destructive == 0 {
		return nil
	}
	rerun := scopedRerunHint(spec, "file-scoped", files)
	if rerun == "" {
		rerun = "rerun with --allow-destructive-scoped --confirm-scope"
	}
	return fmt.Errorf(
		"file-scoped apply blocked by safety gate (destructive actions=%d): %s, or use --dry-run for preflight only",
		destructive,
		rerun,
	)
}

func requireScopedApplyConfirmation(spec *plan.PlanSpec, mode string, selectors []string, confirm, dryRun bool) error {
	if dryRun || confirm || spec == nil {
		return nil
	}
	if mode != "file-scoped" && mode != "targeted" {
		return nil
	}
	mutating := spec.PlanSummary.Create + spec.PlanSummary.Update + spec.PlanSummary.Delete + spec.PlanSummary.Replace + spec.PlanSummary.Forget
	if mutating == 0 {
		return nil
	}
	rerun := scopedRerunHint(spec, mode, selectors)
	if rerun == "" {
		rerun = "rerun with --confirm-scope"
	}
	return fmt.Errorf("%s apply blocked by safety gate (mutating actions=%d): %s, or use --dry-run for preflight only", mode, mutating, rerun)
}

func renderApplyPreflight(w io.Writer, spec *plan.PlanSpec, mode string, selectors []string, dryRun bool, warnings []string) {
	if spec == nil {
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "kl apply preflight")
	fmt.Fprintf(tw, "  mode:\t%s\n", mode)
	fmt.Fprintf(tw, "  state:\t%s\n", spec.StateName)
	fmt.Fprintf(tw, "  config:\t%s\n", spec.ConfigDir)
	fmt.Fprintf(tw, "  write set:\t%d\n", len(spec.WriteSet))
	fmt.Fprintf(tw, "  read set:\t%d\n", len(spec.ReadSet))
	fmt.Fprintf(tw, "  reservations:\t%d\n", len(spec.Reservations))
	if len(spec.WriteSet) > 0 {
		fmt.Fprintf(tw, "  write preview:\t%s\n", strings.Join(previewSlice(spec.WriteSet, scopePreviewLimit), ", "))
	}
	if len(selectors) > 0 {
		fmt.Fprintf(tw, "  selectors:\t%d\n", len(selectors))
	}
	fmt.Fprintf(tw, "  actions:\tcreate=%d update=%d delete=%d replace=%d forget=%d no-op=%d total=%d\n",
		spec.PlanSummary.Create,
		spec.PlanSummary.Update,
		spec.PlanSummary.Delete,
		spec.PlanSummary.Replace,
		spec.PlanSummary.Forget,
		spec.PlanSummary.NoOp,
		spec.PlanSummary.Total,
	)
	if dryRun {
		fmt.Fprintf(tw, "  execution:\tDRY-RUN (terraform apply will not run)\n")
		if rerun := scopedRerunHint(spec, mode, selectors); rerun != "" {
			fmt.Fprintf(tw, "  rerun:\t%s\n", rerun)
		}
	}
	for _, warning := range warnings {
		if strings.TrimSpace(warning) == "" {
			continue
		}
		fmt.Fprintf(tw, "  warning:\t%s\n", warning)
	}
	_ = tw.Flush()
}

func scopedRerunHint(spec *plan.PlanSpec, mode string, selectors []string) string {
	if spec == nil {
		return ""
	}
	if mode != "file-scoped" && mode != "targeted" {
		return ""
	}
	if len(selectors) == 0 {
		return ""
	}
	mutating := spec.PlanSummary.Create + spec.PlanSummary.Update + spec.PlanSummary.Delete + spec.PlanSummary.Replace + spec.PlanSummary.Forget
	if mutating == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("kl apply")
	switch mode {
	case "file-scoped":
		for _, f := range selectors {
			f = strings.TrimSpace(f)
			if f == "" {
				continue
			}
			b.WriteString(" --file=")
			b.WriteString(f)
		}
	case "targeted":
		for _, t := range selectors {
			t = strings.TrimSpace(t)
			if t == "" {
				continue
			}
			b.WriteString(" --target=")
			b.WriteString(t)
		}
		b.WriteString(" --allow-unsafe-target")
	}
	if spec.PlanSummary.Delete+spec.PlanSummary.Replace+spec.PlanSummary.Forget > 0 && mode == "file-scoped" {
		b.WriteString(" --allow-destructive-scoped")
	}
	b.WriteString(" --confirm-scope")
	return b.String()
}

func targetedPreflightWarnings(spec *plan.PlanSpec, targets []string) []string {
	if spec == nil || len(targets) == 0 {
		return nil
	}
	var out []string
	// If the effective writes are much larger than explicit selector count,
	// highlight that scope closure likely pulled more graph branches than
	// expected.
	if len(spec.WriteSet) >= len(targets)*4 && len(spec.WriteSet) >= 8 {
		out = append(out, fmt.Sprintf("target fanout is high: %d writes from %d target selector(s)", len(spec.WriteSet), len(targets)))
	}
	if len(spec.Reservations) >= len(targets)*5 && len(spec.Reservations) >= 10 {
		out = append(out, fmt.Sprintf("reservation fanout is high: %d reservations from %d target selector(s)", len(spec.Reservations), len(targets)))
	}
	return out
}

func enforceTargetPreflightStrict(strict bool, warnings []string) error {
	if !strict || len(warnings) == 0 {
		return nil
	}
	return fmt.Errorf("strict target preflight rejected apply: %d warning(s): %s", len(warnings), strings.Join(warnings, "; "))
}

func fileScopedPreflightWarnings(spec *plan.PlanSpec, files []string, noRefresh bool) []string {
	if spec == nil || len(files) == 0 {
		return nil
	}
	var out []string
	if noRefresh {
		out = append(out, "refresh is disabled for scoped plan (--no-refresh); drift may be missed")
	}
	destructive := spec.PlanSummary.Delete + spec.PlanSummary.Replace + spec.PlanSummary.Forget
	if destructive > 0 {
		out = append(out, fmt.Sprintf("scoped plan contains destructive actions: delete=%d replace=%d forget=%d", spec.PlanSummary.Delete, spec.PlanSummary.Replace, spec.PlanSummary.Forget))
	}
	if len(spec.WriteSet) >= len(files)*50 && len(spec.WriteSet) >= 200 {
		out = append(out, fmt.Sprintf("file scope fanout is high: %d writes from %d file selector(s)", len(spec.WriteSet), len(files)))
	}
	if len(spec.Reservations) >= len(files)*75 && len(spec.Reservations) >= 300 {
		out = append(out, fmt.Sprintf("reservation fanout is high: %d reservations from %d file selector(s)", len(spec.Reservations), len(files)))
	}
	return out
}

func enforceFilePreflightStrict(strict bool, warnings []string) error {
	if !strict || len(warnings) == 0 {
		return nil
	}
	return fmt.Errorf("strict file-scoped preflight rejected apply: %d warning(s): %s", len(warnings), strings.Join(warnings, "; "))
}

func enforceCoexistenceStrict(strict bool, stateName string, locks []store.StatusLock) error {
	if !strict || len(locks) == 0 {
		return nil
	}
	first := locks[0]
	if len(locks) == 1 {
		return fmt.Errorf("strict coexistence rejected apply: state %q has an active vanilla Terraform whole-state lock (lock_id=%s who=%s)", stateName, first.LockID, first.Who)
	}
	return fmt.Errorf("strict coexistence rejected apply: state %q has %d active vanilla Terraform whole-state locks (first lock_id=%s who=%s)", stateName, len(locks), first.LockID, first.Who)
}

// defaultPlanSpecPath returns the default location of the plan
// spec file when --plan-spec is omitted. We deliberately use a
// relative path (resolved against CWD at the time apply runs)
// rather than an absolute path: the engineer flow is to `cd` into
// the module, run `kl plan` (writes ./kl-plan.json
// or whatever --out was), then run `kl apply` in the same
// shell.
func defaultPlanSpecPath() string {
	return filepath.Join(".", "kl-plan.json")
}

func hasTerraformConfigFiles(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasSuffix(name, ".tf") || strings.HasSuffix(name, ".tf.json") {
			return true
		}
	}
	return false
}

// applyTimeout caps how long any single apply invocation can run.
// Generous so large states finish, but bounded so a stuck invocation
// eventually releases its reservations via the orchestrator's
// finalize/release defer chain. Operators with truly long applies
// can override --lease-seconds and rely on the heartbeat to keep the
// lease alive while terraform churns.
const applyTimeout = 60 * time.Minute

// resolveApplyWorkDir picks the working directory for the apply.
// Order of precedence:
//
//  1. Explicit --work-dir flag if non-empty.
//  2. PlanSpec.ConfigDir if non-empty AND it currently exists on
//     this host (the spec may have been produced on a CI runner
//     with a different layout — fall through if the dir is gone).
//  3. The directory containing --plan-spec.
//
// Returns an error only when none of those resolve to a real
// directory: the operator has misconfigured the apply.
func resolveApplyWorkDir(flagDir, specPath string, spec *plan.PlanSpec) (string, error) {
	if flagDir != "" {
		abs, err := filepath.Abs(flagDir)
		if err != nil {
			return "", fmt.Errorf("--work-dir: %w", err)
		}
		if !dirExists(abs) {
			return "", fmt.Errorf("--work-dir=%s: not a directory", abs)
		}
		return abs, nil
	}
	if spec != nil && spec.ConfigDir != "" && dirExists(spec.ConfigDir) {
		abs, err := filepath.Abs(spec.ConfigDir)
		if err != nil {
			return "", fmt.Errorf("spec.config_dir: %w", err)
		}
		return abs, nil
	}
	if specPath != "" {
		abs, err := filepath.Abs(filepath.Dir(specPath))
		if err != nil {
			return "", fmt.Errorf("derive work dir from spec path: %w", err)
		}
		if dirExists(abs) {
			return abs, nil
		}
	}
	return "", errors.New("cannot resolve work directory: pass --work-dir explicitly")
}

func discoverApplyStateName(specPath, workDirFlag string, spec *plan.PlanSpec) string {
	if spec == nil {
		return ""
	}
	backendState := ""
	if workDir, err := resolveApplyWorkDir(workDirFlag, specPath, spec); err == nil {
		if bi, berr := plan.DiscoverBackend(workDir); berr == nil {
			backendState = strings.TrimSpace(bi.StateName)
			if backendState != "" && strings.TrimSpace(spec.StateName) != "" && backendState != strings.TrimSpace(spec.StateName) {
				fmt.Fprintf(os.Stderr, "kl apply: warning: spec.state_name=%q differs from discovered backend state %q; using backend state. Re-run `kl plan` to refresh the spec.\n", spec.StateName, backendState)
			}
		}
	}
	return firstNonEmpty(backendState, spec.StateName)
}

func enforceApplyQuotaPreflight(ctx context.Context, stateName string, spec *plan.PlanSpec, specPath, workDirFlag string) error {
	if spec == nil || strings.TrimSpace(stateName) == "" {
		return nil
	}
	workDir, err := resolveApplyWorkDir(workDirFlag, specPath, spec)
	if err != nil {
		return fmt.Errorf("quota preflight resolve work dir: %w", err)
	}
	client, err := newAPIClientFromBackend(workDir)
	if err != nil {
		return fmt.Errorf("quota preflight discover backend API client: %w", err)
	}
	preview, err := client.checkQuota(ctx, stateName, quotaPlanDeltaFromSummary(spec.PlanSummary))
	if err != nil {
		return formatQuotaPreflightError(err)
	}
	if quotaPreviewExitCode(os.Stderr, preview, "kl apply") != 0 {
		return fmt.Errorf("quota hard limit exceeded")
	}
	return nil
}

func formatQuotaPreflightError(err error) error {
	if err == nil {
		return nil
	}
	msg := strings.ToLower(strings.TrimSpace(err.Error()))
	if strings.Contains(msg, "401 unauthorized") || strings.Contains(msg, `"error":"unauthenticated"`) {
		return fmt.Errorf("quota preflight could not authenticate to the backend admin API; make sure the current backend credentials are also valid for Kilolock admin checks (TF_HTTP_USERNAME/TF_HTTP_PASSWORD, KL_USERNAME/KL_PASSWORD, or KL_TOKEN)")
	}
	return fmt.Errorf("quota preflight failed: %w", err)
}

func getRemoteStateStatus(ctx context.Context, client *apiClient, stateName string) (*store.StateStatus, error) {
	if client == nil || strings.TrimSpace(stateName) == "" {
		return nil, fmt.Errorf("remote state status requires client and state name")
	}
	var status store.StateStatus
	if err := client.doJSON(ctx, "GET", "/admin/state/status?name="+url.QueryEscape(stateName), stateName, nil, &status); err != nil {
		return nil, err
	}
	return &status, nil
}

func planBackendHTTPAuth(bi *plan.BackendInfo) (user, pass string) {
	if bi == nil {
		return "", ""
	}
	if user = strings.TrimSpace(os.Getenv("TF_HTTP_USERNAME")); user == "" {
		user = strings.TrimSpace(os.Getenv("TF_HTTP_USER"))
	}
	if pass = strings.TrimSpace(os.Getenv("TF_HTTP_PASSWORD")); pass != "" || user != "" {
		return user, pass
	}
	return strings.TrimSpace(bi.Username), strings.TrimSpace(bi.Password)
}

func dirExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

// renderApplyResult prints a human-readable summary of the apply.
// Mirrors renderRefreshResult's tabular format: one header line +
// a key/value block of run-level fields, then (when non-empty) a
// sorted list of applied addresses with a hard cap so screens
// don't drown.
func renderApplyResult(w io.Writer, res *apply.Result, runErr error) {
	if res == nil {
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	headline := "apply succeeded"
	if runErr != nil {
		headline = "apply FAILED"
	}
	fmt.Fprintf(tw, "%s\n", headline)
	fmt.Fprintf(tw, "  apply id:\t%s\n", res.ApplyID)
	fmt.Fprintf(tw, "  state:\t%s\n", res.StateName)
	fmt.Fprintf(tw, "  source serial:\t%d\n", res.SourceSerial)
	if runErr == nil {
		fmt.Fprintf(tw, "  committed serial:\t%d\n", res.CommittedSerial)
		fmt.Fprintf(tw, "  new version:\t%s\n", res.NewVersionID)
	}
	fmt.Fprintf(tw, "  resources planned:\t%d\n", res.ResourcesPlanned)
	fmt.Fprintf(tw, "  resources applied:\t%d\n", res.ResourcesApplied)
	if res.ResourcesFailed > 0 {
		fmt.Fprintf(tw, "  resources failed:\t%d\n", res.ResourcesFailed)
	}
	if res.TempDir != "" {
		fmt.Fprintf(tw, "  temp dir:\t%s\n", res.TempDir)
	}
	fmt.Fprintf(tw, "  duration:\t%s\n", res.FinishedAt.Sub(res.StartedAt).Round(time.Millisecond))
	_ = tw.Flush()

	if len(res.AppliedAddresses) > 0 {
		const max = 25
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Applied:")
		for i, a := range res.AppliedAddresses {
			if i >= max {
				fmt.Fprintf(w, "  ... and %d more\n", len(res.AppliedAddresses)-max)
				break
			}
			fmt.Fprintf(w, "  %s\n", a)
		}
	}

	if runErr != nil {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Error:", formatApplyRunError(runErr))
	}
}

func formatApplyRunError(err error) string {
	if err == nil {
		return ""
	}
	var conflict *store.ReservationConflictError
	if !errors.As(err, &conflict) {
		return err.Error()
	}
	return formatReservationConflictError(conflict)
}

func formatReservationConflictError(conflict *store.ReservationConflictError) string {
	if conflict == nil || len(conflict.Conflicts) == 0 {
		return "reservation conflict"
	}
	grouped := make(map[string][]store.ActiveReservation)
	for _, reservation := range conflict.Conflicts {
		grouped[reservation.ApplyID] = append(grouped[reservation.ApplyID], reservation)
	}
	type applyBlocker struct {
		applyID   string
		holder    string
		expiresAt time.Time
		conflicts []store.ActiveReservation
	}
	blockers := make([]applyBlocker, 0, len(grouped))
	for applyID, conflicts := range grouped {
		sort.Slice(conflicts, func(i, j int) bool {
			if conflicts[i].AddressGlob != conflicts[j].AddressGlob {
				return conflicts[i].AddressGlob < conflicts[j].AddressGlob
			}
			return conflicts[i].Mode < conflicts[j].Mode
		})
		holder := strings.TrimSpace(conflicts[0].Holder)
		expiresAt := conflicts[0].ExpiresAt
		for _, reservation := range conflicts[1:] {
			if holder == "" && strings.TrimSpace(reservation.Holder) != "" {
				holder = strings.TrimSpace(reservation.Holder)
			}
			if reservation.ExpiresAt.After(expiresAt) {
				expiresAt = reservation.ExpiresAt
			}
		}
		blockers = append(blockers, applyBlocker{
			applyID:   applyID,
			holder:    holder,
			expiresAt: expiresAt,
			conflicts: conflicts,
		})
	}
	sort.Slice(blockers, func(i, j int) bool {
		if blockers[i].expiresAt.Equal(blockers[j].expiresAt) {
			return blockers[i].applyID < blockers[j].applyID
		}
		return blockers[i].expiresAt.Before(blockers[j].expiresAt)
	})

	var b strings.Builder
	currentActor := ""
	allSameHolder := true
	for i, blocker := range blockers {
		holder := strings.TrimSpace(blocker.holder)
		if i == 0 {
			currentActor = holder
			continue
		}
		if holder != currentActor {
			allSameHolder = false
			break
		}
	}
	switch {
	case currentActor != "" && allSameHolder:
		fmt.Fprintf(&b, "acquire reservations blocked by your previous apply run(s) as %s:", currentActor)
	case currentActor != "" && len(blockers) == 1:
		fmt.Fprintf(&b, "acquire reservations blocked by 1 active apply run held by %s:", currentActor)
	default:
		fmt.Fprintf(&b, "acquire reservations blocked by %d active apply run(s):", len(blockers))
	}
	for _, blocker := range blockers {
		localExpiry := blocker.expiresAt.In(time.Local)
		until := time.Until(localExpiry).Round(time.Second)
		if until < 0 {
			until = 0
		}
		holder := blocker.holder
		if holder == "" {
			holder = "unknown"
		}
		fmt.Fprintf(&b, "\n  - apply %s held by %s; %d conflicting reservation(s); expires %s (%s from now)",
			blocker.applyID,
			holder,
			len(blocker.conflicts),
			localExpiry.Format(time.RFC3339),
			until,
		)
		for i, reservation := range blocker.conflicts {
			if i >= 5 {
				fmt.Fprintf(&b, "\n      ... and %d more", len(blocker.conflicts)-5)
				break
			}
			fmt.Fprintf(&b, "\n      %s [%s]", reservation.AddressGlob, reservation.Mode)
		}
		fmt.Fprintf(&b, "\n      To clear it now: kl apply abort --apply-id %s", blocker.applyID)
	}
	if currentActor != "" && allSameHolder {
		fmt.Fprintf(&b, "\n  Hint: this usually means a previous apply was interrupted after acquiring reservations but before cleanup.")
	}
	return b.String()
}
