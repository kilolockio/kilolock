package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/kilolockio/kilolock/pkg/config"
	"github.com/kilolockio/kilolock/pkg/store"
)

const stateUsage = `Usage:
  kl state rm [state] --address ADDR [flags]
  kl state mv [state] --from ADDR --to ADDR [flags]

Native state-engine mode:
  When KL_PROTOCOL=state-engine (or .kl.toml sets protocol = "state-engine"),
  kl uses native exact-address preview/apply endpoints that lock the state
  against plain Terraform while the mutation commits.

Terraform compatibility mode:
  Otherwise kl falls back to:
    terraform state rm ...
    terraform state mv ...
  in the selected working directory.

Address / auth precedence:
  1. --state-url
  2. KL_STATE_URL
  3. current directory backend config

  Token precedence:
  1. --token
  2. KL_TOKEN
  3. backend auth / TF_HTTP_* env vars
`

func runState(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, stateUsage)
		return 2
	}
	switch strings.TrimSpace(args[0]) {
	case "rm", "remove":
		return runStateRemove(args[1:])
	case "mv", "move":
		return runStateMove(args[1:])
	case "help", "--help", "-h":
		fmt.Fprint(os.Stdout, stateUsage)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "kl state: unknown subcommand %q\n\n", args[0])
		fmt.Fprint(os.Stderr, stateUsage)
		return 2
	}
}

func runStateRemove(args []string) int {
	fs := flag.NewFlagSet("state rm", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var (
		address      = fs.String("address", "", "Exact Terraform resource address to remove.")
		apply        = fs.Bool("apply", false, "Actually perform the mutation. Default is preview only in state-engine mode.")
		format       = fs.String("format", "table", "Output format for native preview/apply: table|json.")
		timeout      = fs.Duration("timeout", resourceQueryTimeout, "Request timeout for native preview/apply (e.g. 30s, 2m).")
		yes          = fs.Bool("yes", false, "Skip interactive confirmation for native apply.")
		terraformBin = fs.String("terraform-bin", "", "Path to the terraform binary for compatibility mode.")
		iacVersion   = fs.String("iac-version", "", "Desired IaC CLI version for compatibility mode.")
		workDir      = fs.String("work-dir", ".", "Terraform working directory for compatibility mode.")
	)
	fs.BoolVar(yes, "y", false, "Alias for --yes.")
	adminFlags := registerAdminClientFlags(fs, true)
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "kl state rm:", err)
		return 2
	}
	if fs.NArg() > 1 {
		fmt.Fprintln(os.Stderr, "kl state rm: too many positional arguments")
		return 2
	}
	if strings.TrimSpace(*address) == "" {
		fmt.Fprintln(os.Stderr, "kl state rm: --address is required")
		return 2
	}

	target, _, err := adminFlags.resolveStateTarget(fs.Arg(0), ".")
	if err != nil {
		fmt.Fprintln(os.Stderr, "kl state rm:", err)
		return 2
	}
	if resolvedCLIProtocol(config.Load()) == cliProtocolStateEngine {
		return runStateEngineRemove(target, adminFlags, strings.TrimSpace(*address), *apply, *yes, *format, *timeout)
	}
	return runTerraformStateRemove(target, adminFlags, strings.TrimSpace(*address), *workDir, *terraformBin, *iacVersion)
}

func runStateMove(args []string) int {
	fs := flag.NewFlagSet("state mv", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var (
		from         = fs.String("from", "", "Exact Terraform resource address to move from.")
		to           = fs.String("to", "", "Exact Terraform resource address to move to.")
		apply        = fs.Bool("apply", false, "Actually perform the mutation. Default is preview only in state-engine mode.")
		format       = fs.String("format", "table", "Output format for native preview/apply: table|json.")
		timeout      = fs.Duration("timeout", resourceQueryTimeout, "Request timeout for native preview/apply (e.g. 30s, 2m).")
		yes          = fs.Bool("yes", false, "Skip interactive confirmation for native apply.")
		terraformBin = fs.String("terraform-bin", "", "Path to the terraform binary for compatibility mode.")
		iacVersion   = fs.String("iac-version", "", "Desired IaC CLI version for compatibility mode.")
		workDir      = fs.String("work-dir", ".", "Terraform working directory for compatibility mode.")
	)
	fs.BoolVar(yes, "y", false, "Alias for --yes.")
	adminFlags := registerAdminClientFlags(fs, true)
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "kl state mv:", err)
		return 2
	}
	if fs.NArg() > 1 {
		fmt.Fprintln(os.Stderr, "kl state mv: too many positional arguments")
		return 2
	}
	if strings.TrimSpace(*from) == "" || strings.TrimSpace(*to) == "" {
		fmt.Fprintln(os.Stderr, "kl state mv: --from and --to are required")
		return 2
	}

	target, _, err := adminFlags.resolveStateTarget(fs.Arg(0), ".")
	if err != nil {
		fmt.Fprintln(os.Stderr, "kl state mv:", err)
		return 2
	}
	if resolvedCLIProtocol(config.Load()) == cliProtocolStateEngine {
		return runStateEngineMove(target, adminFlags, strings.TrimSpace(*from), strings.TrimSpace(*to), *apply, *yes, *format, *timeout)
	}
	return runTerraformStateMove(target, adminFlags, strings.TrimSpace(*from), strings.TrimSpace(*to), *workDir, *terraformBin, *iacVersion)
}

func runStateEngineRemove(target stateTarget, adminFlags adminClientFlags, address string, apply, yes bool, format string, timeout time.Duration) int {
	client, err := adminFlags.newClient(".")
	if err != nil {
		fmt.Fprintln(os.Stderr, "kl state rm:", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(cliContext(), timeout)
	defer cancel()
	engine := newStateEngineClient(client)
	preview, err := engine.previewRemove(ctx, target.StateName, address)
	if err != nil {
		fmt.Fprintln(os.Stderr, "kl state rm:", err)
		return 1
	}
	if err := renderStateMutationPreview(os.Stdout, format, preview); err != nil {
		fmt.Fprintln(os.Stderr, "kl state rm:", err)
		return 1
	}
	if !apply || preview.Action == "no-op" {
		if format != "json" {
			if preview.Action == "no-op" {
				fmt.Fprintln(os.Stdout, "No-op: current state does not contain the selected address.")
			} else {
				fmt.Fprintln(os.Stdout, "Dry-run only. Re-run with --apply to write a new state version.")
			}
		}
		return 0
	}
	if !yes && !confirmResourceRollback(os.Stdin, os.Stdout, address) {
		fmt.Fprintln(os.Stderr, "kl state rm: cancelled by operator")
		return 2
	}
	resp, err := engine.applyRemove(ctx, target.StateName, address, cliActor())
	if err != nil {
		fmt.Fprintln(os.Stderr, "kl state rm:", err)
		return 1
	}
	return renderStateMutationApplyResult(os.Stdout, "rm", format, resp)
}

func runStateEngineMove(target stateTarget, adminFlags adminClientFlags, from, to string, apply, yes bool, format string, timeout time.Duration) int {
	client, err := adminFlags.newClient(".")
	if err != nil {
		fmt.Fprintln(os.Stderr, "kl state mv:", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(cliContext(), timeout)
	defer cancel()
	engine := newStateEngineClient(client)
	preview, err := engine.previewMove(ctx, target.StateName, from, to)
	if err != nil {
		fmt.Fprintln(os.Stderr, "kl state mv:", err)
		return 1
	}
	if err := renderStateMutationPreview(os.Stdout, format, preview); err != nil {
		fmt.Fprintln(os.Stderr, "kl state mv:", err)
		return 1
	}
	if !apply || preview.Action == "no-op" {
		if format != "json" {
			switch preview.Action {
			case "no-op":
				fmt.Fprintln(os.Stdout, "No-op: current state does not contain the selected source address.")
			case "conflict":
				fmt.Fprintln(os.Stdout, "Move blocked: destination address already exists in the current state.")
			default:
				fmt.Fprintln(os.Stdout, "Dry-run only. Re-run with --apply to write a new state version.")
			}
		}
		return 0
	}
	if !yes && !confirmResourceRollback(os.Stdin, os.Stdout, from) {
		fmt.Fprintln(os.Stderr, "kl state mv: cancelled by operator")
		return 2
	}
	resp, err := engine.applyMove(ctx, target.StateName, from, to, cliActor())
	if err != nil {
		fmt.Fprintln(os.Stderr, "kl state mv:", err)
		return 1
	}
	return renderStateMutationApplyResult(os.Stdout, "mv", format, resp)
}

func runTerraformStateRemove(target stateTarget, adminFlags adminClientFlags, address, workDir, terraformBin, iacVersion string) int {
	if err := runTerraformStateCommand(cliContext(), target, adminFlags, workDir, terraformBin, iacVersion, "rm", address); err != nil {
		fmt.Fprintln(os.Stderr, "kl state rm:", err)
		return 1
	}
	return 0
}

func runTerraformStateMove(target stateTarget, adminFlags adminClientFlags, from, to, workDir, terraformBin, iacVersion string) int {
	if err := runTerraformStateCommand(cliContext(), target, adminFlags, workDir, terraformBin, iacVersion, "mv", from, to); err != nil {
		fmt.Fprintln(os.Stderr, "kl state mv:", err)
		return 1
	}
	return 0
}

func runTerraformStateCommand(ctx context.Context, target stateTarget, adminFlags adminClientFlags, workDir, terraformBin, iacVersion, subcommand string, args ...string) error {
	workDir = strings.TrimSpace(workDir)
	if workDir == "" {
		workDir = "."
	}
	absWorkDir, err := filepath.Abs(workDir)
	if err != nil {
		return fmt.Errorf("resolve work dir: %w", err)
	}
	icfg := config.Load()
	resolvedBin, err := resolveIACBinary(terraformBin, iacVersion, icfg.IACBinary, icfg.IACVersion)
	if err != nil {
		return err
	}
	cmdArgs := append([]string{"state", subcommand}, args...)
	cmd := exec.CommandContext(ctx, resolvedBin, cmdArgs...)
	cmd.Dir = absWorkDir
	cmd.Env = terraformStateCommandEnv(target, adminFlags)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", resolvedBin, strings.Join(cmdArgs, " "), err)
	}
	return nil
}

func terraformStateCommandEnv(target stateTarget, adminFlags adminClientFlags) []string {
	env := os.Environ()
	if addr := resolvedStateCommandAddress(target, adminFlags); addr != "" {
		env = append(env, "TF_HTTP_ADDRESS="+addr)
	}
	if token := adminFlags.explicitToken(); token != "" {
		env = append(env, "TF_HTTP_PASSWORD="+token)
	} else if token := strings.TrimSpace(os.Getenv("KL_TOKEN")); token != "" {
		env = append(env, "TF_HTTP_PASSWORD="+token)
	} else if strings.TrimSpace(target.Password) != "" {
		env = append(env, "TF_HTTP_PASSWORD="+strings.TrimSpace(target.Password))
	}
	if user := strings.TrimSpace(target.Username); user != "" {
		env = append(env, "TF_HTTP_USERNAME="+user)
	}
	return env
}

func resolvedStateCommandAddress(target stateTarget, adminFlags adminClientFlags) string {
	if addr := strings.TrimSpace(adminFlags.explicitStateURL()); addr != "" {
		return addr
	}
	if addr := strings.TrimSpace(os.Getenv("KL_STATE_URL")); addr != "" {
		return addr
	}
	bi, err := discoverLiveBackend(".")
	if err == nil && strings.TrimSpace(bi.Address) != "" {
		return strings.TrimSpace(bi.Address)
	}
	return ""
}

func renderStateMutationPreview(w io.Writer, format string, preview *store.ResourceMutationPreview) error {
	if preview == nil {
		return fmt.Errorf("backend returned no preview")
	}
	switch format {
	case "json":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		return enc.Encode(preview)
	case "table", "":
		fmt.Fprintf(w, "state:   %s\n", preview.StateName)
		fmt.Fprintf(w, "action:  %s\n", preview.Action)
		fmt.Fprintf(w, "address: %s\n", preview.Address)
		if strings.TrimSpace(preview.ToAddress) != "" {
			fmt.Fprintf(w, "to:      %s\n", preview.ToAddress)
		}
		fmt.Fprintf(w, "serial:  %d\n", preview.CurrentVersion.Serial)
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintf(tw, "\n  current exists:\t%t\n", preview.CurrentExists)
		fmt.Fprintf(tw, "  target exists:\t%t\n", preview.TargetExists)
		if len(preview.Dependencies) > 0 {
			fmt.Fprintf(tw, "  dependencies:\t%s\n", strings.Join(preview.Dependencies, ", "))
		}
		if len(preview.Dependents) > 0 {
			fmt.Fprintf(tw, "  dependents:\t%s\n", strings.Join(preview.Dependents, ", "))
		}
		_ = tw.Flush()
		if len(preview.Warnings) > 0 {
			fmt.Fprintln(w, "\nwarnings:")
			for _, warning := range preview.Warnings {
				fmt.Fprintf(w, "  - %s\n", warning)
			}
		}
		return nil
	default:
		return fmt.Errorf("--format must be table or json")
	}
}

func renderStateMutationApplyResult(w io.Writer, verb, format string, resp *stateEngineMutationResponse) int {
	if resp == nil || resp.Preview == nil {
		fmt.Fprintf(os.Stderr, "kl state %s: backend did not return mutation preview metadata\n", verb)
		return 1
	}
	if format == "json" {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		if err := enc.Encode(resp); err != nil {
			fmt.Fprintf(os.Stderr, "kl state %s: %v\n", verb, err)
			return 1
		}
		return 0
	}
	if resp.Version == nil {
		fmt.Fprintf(os.Stderr, "kl state %s: backend did not return new version metadata\n", verb)
		return 1
	}
	fmt.Fprintf(w, "\nState mutation applied.\n  new serial: %d\n  new version id: %s\n", resp.Version.Serial, resp.Version.ID)
	return 0
}
