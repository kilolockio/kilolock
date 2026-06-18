package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/kilolockio/kilolock/internal/plan"
	"github.com/kilolockio/kilolock/internal/slice"
	"github.com/kilolockio/kilolock/pkg/config"
)

// varFlag implements flag.Value for the repeatable --var=NAME=VALUE
// argument. Each value is JSON-encoded into a RawMessage so
// downstream code can splice it into a `-var=` argument uniformly
// across HCL types. CLI input is always taken as a plain string
// (operators don't have to remember to quote things); the encoder
// runs through strconv.Quote to produce `"the value"`.
//
// Duplicate keys overwrite, matching terraform's own precedence
// (later --var wins).
type varFlag struct {
	values map[string]json.RawMessage
}

// fileFlag implements flag.Value for repeatable --file selectors.
type fileFlag struct {
	values []string
}

func (f *fileFlag) Set(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return fmt.Errorf("--file expects a non-empty path")
	}
	f.values = append(f.values, raw)
	return nil
}

func (f *fileFlag) String() string {
	if f == nil || len(f.values) == 0 {
		return ""
	}
	return strings.Join(f.values, ",")
}

// targetFlag implements flag.Value for repeatable --target selectors.
type targetFlag struct {
	values []string
}

func (t *targetFlag) Set(raw string) error {
	s := strings.TrimSpace(raw)
	if s == "" {
		return fmt.Errorf("--target expects a non-empty address")
	}
	for _, v := range t.values {
		if v == s {
			return nil
		}
	}
	t.values = append(t.values, s)
	return nil
}

func (t *targetFlag) String() string {
	if t == nil || len(t.values) == 0 {
		return ""
	}
	return strings.Join(t.values, ",")
}

// Set parses one NAME=VALUE pair. NAME must be non-empty and may
// not contain '='; VALUE is taken verbatim (including internal
// '=' chars) and JSON-encoded as a string.
func (v *varFlag) Set(raw string) error {
	idx := strings.IndexByte(raw, '=')
	if idx <= 0 {
		return fmt.Errorf("--var expects NAME=VALUE (got %q)", raw)
	}
	name := raw[:idx]
	value := raw[idx+1:]
	if v.values == nil {
		v.values = make(map[string]json.RawMessage)
	}
	v.values[name] = plan.EncodeStringVar(value)
	return nil
}

// String renders the current map as a comma-separated list of
// NAME=<json> pairs. The format only ever shows up in --help
// output; correctness is secondary to readability.
func (v *varFlag) String() string {
	if v == nil || len(v.values) == 0 {
		return ""
	}
	parts := make([]string, 0, len(v.values))
	for k, val := range v.values {
		parts = append(parts, k+"="+string(val))
	}
	return strings.Join(parts, ",")
}

// planUsage is printed on argv errors and `kl plan --help`.
//
// The "no DB access required" note matters: operators evaluating
// kl against a vanilla Terraform workflow don't have to set
// up the database to try the plan introspection. That lowers the
// bar for "what does this thing think my plan does?".
const planUsage = `Usage: kl plan [config-dir] [flags]

Runs ` + "`terraform plan -out=<tmp> -input=false`" + ` inside the config
directory, inspects the resulting plan with ` + "`terraform show -json`" + `,
and writes a kl plan spec to disk. The spec is the input
to ` + "`kl apply`" + ` (v2c) and describes the predicted write set,
read set, dependency graph, reservation list, the state name (from
the http backend's address), and the effective input-variable set
terraform's planner used.

Optional positional:
  [config-dir]            Terraform working directory (must be ` + "`terraform init`" + `-ed).
                          Defaults to the current working directory.

Flags:
  --out PATH, -o PATH     Where to write the plan spec. Defaults to
                          <config-dir>/kl-plan.json.
                          Use - for stdout.
  --terraform-bin PATH    terraform binary path. Default: "terraform" on $PATH.
  --no-lock               Pass -lock=false to terraform plan (skip backend lock).
  --no-refresh            Pass -refresh=false to terraform plan. Skips the
                          per-resource provider Read phase that dominates
                          plan latency on large states. Use when you trust
                          the trunk to be current (iterative edits, demos);
                          drift against real-world infrastructure will not
                          be detected. Default false (refresh is performed).
  --var NAME=VALUE        Override a terraform input variable for this plan
                          (repeatable). Always wins over what terraform
                          would otherwise read from TF_VAR_* or .tfvars.
                          The pair is pinned into the spec so kl
                          apply replays it verbatim.
  --no-pin-vars           Don't snapshot the effective input-variable set
                          (TF_VAR_*, terraform.tfvars, *.auto.tfvars, etc.)
                          into the plan spec. Use when some plan-time
                          variables are sensitive enough that they should
                          not land in PR diffs or CI artifacts. Explicit
                          --var values are still pinned. The apply will
                          then rely on the same env / tfvars sources being
                          present at apply time.
  --file PATH, -f PATH    Scope the plan/apply write ownership to resources
                          declared in PATH. Repeatable. Path is resolved
                          relative to [config-dir].
  --target ADDR           Terraform target address (repeatable). In v1 this
                          uses Terraform target semantics; state-first closure
                          from ADR-0017 lands in a later phase.
  --timeout DUR           Maximum wall time for plan + show (default 10m).

Exit codes:
  0  spec written
  1  terraform plan or terraform show failed, or spec write failed
  2  argv / usage error

Target guard (CI):
  Set ` + "`KL_TARGET_MAX_WRITES`" + ` and/or
  ` + "`KL_TARGET_MAX_RESERVATIONS`" + ` to fail ` + "`plan --target`" + `
  when fanout exceeds your pipeline safety limits.

No database connection is required for full-repository planning.
` + "`--file`" + ` uses the current trunk state from the configured HTTP backend
and therefore requires a reachable Kilolock server.
`

// planTimeoutDefault is the wall-time budget for the combined
// terraform plan + show invocations. Large states (10k+ resources)
// can take 5+ minutes to plan; 10m is generous but not absurd for
// the worst case operators will hit during the v2b release.
const planTimeoutDefault = 10 * time.Minute

// runPlan is the entrypoint for `kl plan <config-dir>`.
func runPlan(args []string) int {
	fs := flag.NewFlagSet("plan", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // we render usage ourselves on parse error

	var (
		out          string
		terraformBin = fs.String("terraform-bin", "", `terraform binary path (default "terraform" on $PATH).`)
		iacVersion   = fs.String("iac-version", "", "Desired IaC CLI version (used with --terraform-bin / KL_IAC_BIN).")
		noLock       = fs.Bool("no-lock", false, "Pass -lock=false to terraform plan.")
		noRefresh    = fs.Bool("no-refresh", false, "Pass -refresh=false to terraform plan (skip drift detection; fast path for iterative edits).")
		noPinVars    = fs.Bool("no-pin-vars", false, "Don't pin the effective TF input-variable set (TF_VAR_*, tfvars) into the spec; only explicit --var overrides are pinned.")
		timeout      = fs.Duration("timeout", planTimeoutDefault, "Maximum wall time for plan+show.")
	)
	registerStringFlagAlias(fs, &out, "out", "o", "", "Output path for the plan spec (default: <config-dir>/kl-plan.json, - for stdout).")
	files := &fileFlag{}
	registerFlagValueAlias(fs, files, "file", "f", "Scope to resources declared in this file (repeatable).")
	targets := &targetFlag{}
	fs.Var(targets, "target", "Terraform target address (repeatable).")
	vars := &varFlag{}
	fs.Var(vars, "var", "Override a terraform variable as NAME=VALUE (repeatable; wins over TF_VAR_* and tfvars). Pinned into the plan spec.")

	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "kl plan:", err)
		fmt.Fprint(os.Stderr, planUsage)
		return 2
	}
	if fs.NArg() > 1 {
		fmt.Fprintf(os.Stderr, "kl plan: unexpected extra arguments: %v\n", fs.Args()[1:])
		return 2
	}
	if len(files.values) > 0 && len(targets.values) > 0 {
		fmt.Fprintln(os.Stderr, "kl plan: use either --file or --target, not both")
		return 2
	}
	// config-dir defaults to CWD when omitted: matches the engineer
	// workflow of `cd` into the module and run `kl plan` with
	// no other ceremony.
	configDir := fs.Arg(0)
	if configDir == "" {
		configDir = "."
	}

	absConfigDir, err := filepath.Abs(configDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "kl plan: resolve config dir: %v\n", err)
		return 2
	}
	if info, err := os.Stat(absConfigDir); err != nil {
		fmt.Fprintf(os.Stderr, "kl plan: stat %s: %v\n", absConfigDir, err)
		return 2
	} else if !info.IsDir() {
		fmt.Fprintf(os.Stderr, "kl plan: %s is not a directory\n", absConfigDir)
		return 2
	}

	outPath, err := resolvePlanOutPath(out, absConfigDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "kl plan:", err)
		return 2
	}

	ctx, cancel := context.WithTimeout(cliContext(), *timeout)
	defer cancel()
	icfg := config.Load()
	resolvedBin, berr := resolveIACBinary(*terraformBin, *iacVersion, icfg.IACBinary, icfg.IACVersion)
	if berr != nil {
		fmt.Fprintln(os.Stderr, "kl plan:", berr)
		return 2
	}

	// Backend discovery is best-effort: if the directory isn't
	// terraform-init'ed against an http backend (e.g. a fresh
	// checkout, a local-backend test, or a non-http backend) we
	// just leave StateName empty and let `kl apply`
	// require an explicit --state=…. Surface the discovery
	// outcome to stderr so the operator knows what got pinned.
	var (
		stateName string
		backend   *plan.BackendInfo
		trunkRaw  []byte
		srcSerial *int64
	)
	if bi, berr := plan.DiscoverBackend(absConfigDir); berr == nil {
		backend = bi
		stateName = bi.StateName
		fmt.Fprintf(os.Stderr, "kl plan: state %q discovered from backend (%s)\n", stateName, bi.Address)

		// Best-effort fetch of the current trunk to pin source_serial.
		// This lets apply reject stale plans even when the plan itself is
		// a full-config plan (no slicing required).
		if raw, rerr := plan.FetchCurrentStateFromBackend(ctx, backend); rerr == nil {
			trunkRaw = raw
			if ts, perr := slice.ParseTrunkState(raw); perr == nil && ts.Serial > 0 {
				v := ts.Serial
				srcSerial = &v
			}
		} else {
			fmt.Fprintf(os.Stderr, "kl plan: warning: failed to fetch trunk state for source_serial pinning: %v\n", rerr)
		}
	} else if errors.Is(berr, plan.ErrUnsupportedBackend) {
		fmt.Fprintf(os.Stderr, "kl plan: %v (apply will require --state=…)\n", berr)
	}

	var jsonBytes []byte
	if len(files.values) > 0 {
		if stateName == "" {
			fmt.Fprintln(os.Stderr, "kl plan: --file requires a terraform-init'ed http backend so the trunk state can be sliced")
			return 2
		}
		raw := trunkRaw
		if len(raw) == 0 {
			var err error
			raw, err = plan.FetchCurrentStateFromBackend(ctx, backend)
			if err != nil {
				fmt.Fprintln(os.Stderr, "kl plan: read trunk state from backend:", err)
				return 1
			}
		}
		scope, err := plan.NormalizeFileScope(absConfigDir, files.values)
		if err != nil {
			fmt.Fprintln(os.Stderr, "kl plan:", err)
			return 2
		}
		planMsg := "kl plan: running scoped terraform plan…"
		if *noRefresh {
			planMsg += " (refresh disabled)"
		}
		fmt.Fprintln(os.Stderr, planMsg)
		jsonBytes, err = plan.RunScopedTerraformPlan(ctx, resolvedBin, absConfigDir, raw, scope, plan.ScopedPlanOptions{
			Refresh: !*noRefresh,
			Vars:    vars.values,
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "kl plan:", err)
			return 1
		}
	} else if len(targets.values) > 0 {
		if stateName == "" {
			fmt.Fprintln(os.Stderr, "kl plan: --target requires a terraform-init'ed http backend so the trunk state can be sliced")
			return 2
		}
		raw := trunkRaw
		if len(raw) == 0 {
			var err error
			raw, err = plan.FetchCurrentStateFromBackend(ctx, backend)
			if err != nil {
				fmt.Fprintln(os.Stderr, "kl plan: read trunk state from backend:", err)
				return 1
			}
		}
		planMsg := "kl plan: running targeted scoped terraform plan…"
		if *noRefresh {
			planMsg += " (refresh disabled)"
		}
		fmt.Fprintln(os.Stderr, planMsg)
		jsonBytes, err = plan.RunTargetScopedTerraformPlan(ctx, resolvedBin, absConfigDir, raw, targets.values, plan.ScopedPlanOptions{
			Refresh: !*noRefresh,
			Vars:    vars.values,
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "kl plan:", err)
			return 1
		}
		parsed, perr := plan.ParseShowJSONBytes(jsonBytes)
		if perr != nil {
			fmt.Fprintln(os.Stderr, "kl plan: parse targeted plan JSON:", perr)
			return 1
		}
		targetSpec := plan.BuildSpec(parsed, plan.SpecBuildInput{
			ConfigDir:    absConfigDir,
			GeneratedAt:  time.Now().UTC(),
			StateName:    stateName,
			SourceSerial: srcSerial,
			ExplicitVars: vars.values,
			PinAllVars:   !*noPinVars,
		})
		allowed, aerr := plan.ExpandTargetSliceAddresses(absConfigDir, targets.values)
		if aerr != nil {
			fmt.Fprintln(os.Stderr, "kl plan:", aerr)
			return 1
		}
		if verr := plan.ValidateTargetedWriteSet(targetSpec.WriteSet, allowed); verr != nil {
			fmt.Fprintln(os.Stderr, "kl plan:", formatTargetScopeViolation(verr, targets.values, targetSpec.WriteSet))
			return 1
		}
		if gerr := targetGuardViolation(targetSpec, targets.values); gerr != nil {
			fmt.Fprintln(os.Stderr, "kl plan:", gerr)
			return 1
		}
	} else {
		// Write the binary tfplan to a tmp file in the config dir so
		// terraform's plugin cache + provider lookup work. Cleanup runs
		// even on failure; the file is small (a few KB to MB) but
		// leaving it around would pollute the user's working tree.
		tfplanPath, err := os.CreateTemp(absConfigDir, ".kl-plan-*.tfplan")
		if err != nil {
			fmt.Fprintf(os.Stderr, "kl plan: create tmp plan file: %v\n", err)
			return 1
		}
		tfplanPath.Close()
		defer os.Remove(tfplanPath.Name())

		planMsg := "kl plan: running terraform plan…"
		if *noRefresh {
			planMsg += " (refresh disabled)"
		}
		fmt.Fprintln(os.Stderr, planMsg)
		if err := plan.RunTerraformPlan(ctx, resolvedBin, absConfigDir, tfplanPath.Name(), plan.PlanRunOptions{
			Lock:    !*noLock,
			Refresh: !*noRefresh,
			Vars:    vars.values,
			Targets: targets.values,
		}); err != nil {
			fmt.Fprintln(os.Stderr, "kl plan:", err)
			return 1
		}

		jsonBytes, err = plan.RunTerraformShow(ctx, resolvedBin, absConfigDir, tfplanPath.Name())
		if err != nil {
			fmt.Fprintln(os.Stderr, "kl plan:", err)
			return 1
		}
	}

	rc, spec, err := emitPlanSpec(planEmitInput{
		showJSON:     jsonBytes,
		configDir:    absConfigDir,
		stateName:    stateName,
		sourceSerial: srcSerial,
		scopeFiles:   files.values,
		outPath:      outPath,
		generatedAt:  time.Now().UTC(),
		explicitVars: vars.values,
		pinAllVars:   !*noPinVars,
		stdout:       os.Stdout,
		stderr:       os.Stderr,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "kl plan:", err)
	}
	if rc == 0 && stateName != "" && spec != nil {
		client, cerr := newAPIClientFromBackend(absConfigDir)
		if cerr != nil {
			fmt.Fprintf(os.Stderr, "kl plan: quota check failed: %v\n", cerr)
			return 1
		}
		preview, qerr := client.checkQuota(ctx, stateName, quotaPlanDeltaFromSummary(spec.PlanSummary))
		if qerr != nil {
			fmt.Fprintf(os.Stderr, "kl plan: quota check failed: %v\n", qerr)
			return 1
		}
		if qrc := quotaPreviewExitCode(os.Stderr, preview, "kl plan"); qrc != 0 {
			return qrc
		}
	}
	return rc
}

// planEmitInput separates the deterministic spec-building work from
// the terraform-binary-invocation side of runPlan. Tests exercise
// emitPlanSpec directly with captured `terraform show -json` bytes,
// so the e2e path stays exercised without spinning up terraform.
type planEmitInput struct {
	showJSON     []byte
	configDir    string
	stateName    string
	sourceSerial *int64
	scopeFiles   []string
	outPath      string // "-" for stdout, otherwise filesystem path
	generatedAt  time.Time
	explicitVars map[string]json.RawMessage
	pinAllVars   bool
	stdout       io.Writer
	stderr       io.Writer
}

// emitPlanSpec parses the show JSON, builds a PlanSpec, marshals it
// to bytes, and writes it to outPath (or stdout when outPath == "-").
// Returns the process exit code that runPlan should use (0 success,
// 1 failure) plus any error to surface on stderr.
func emitPlanSpec(in planEmitInput) (int, *plan.PlanSpec, error) {
	parsed, err := plan.ParseShowJSONBytes(in.showJSON)
	if err != nil {
		return 1, nil, fmt.Errorf("parse: %w", err)
	}
	_ = plan.UpdateOwnershipCache(in.configDir, parsed)
	spec := plan.BuildSpec(parsed, plan.SpecBuildInput{
		ConfigDir:    in.configDir,
		GeneratedAt:  in.generatedAt,
		StateName:    in.stateName,
		SourceSerial: in.sourceSerial,
		ExplicitVars: in.explicitVars,
		PinAllVars:   in.pinAllVars,
	})
	scope, err := plan.NormalizeFileScope(in.configDir, in.scopeFiles)
	if err != nil {
		return 1, nil, err
	}
	spec, err = plan.ApplyFileScope(parsed, spec, scope)
	if err != nil {
		return 1, nil, err
	}
	if len(in.scopeFiles) > 0 && len(spec.WriteSet) == 0 {
		return 1, spec, formatFileScopeEmptyWriteSet(parsed, spec, scope)
	}
	specBytes, err := plan.MarshalSpec(spec)
	if err != nil {
		return 1, spec, fmt.Errorf("marshal: %w", err)
	}
	if in.outPath == "-" {
		if _, err := in.stdout.Write(append(specBytes, '\n')); err != nil {
			return 1, spec, fmt.Errorf("write stdout: %w", err)
		}
	} else {
		if err := os.WriteFile(in.outPath, specBytes, 0o644); err != nil {
			return 1, spec, fmt.Errorf("write spec: %w", err)
		}
		fmt.Fprintf(in.stderr, "kl plan: spec written to %s\n", in.outPath)
	}
	renderPlanSummary(in.stderr, spec)
	return 0, spec, nil
}

// resolvePlanOutPath derives the effective output path from the
// --out flag and the config dir. Empty defaults to a file next to
// the HCL; - means stdout. Absolute paths are used verbatim;
// relative paths are resolved against the current working
// directory (not the config dir) — operators typically run plan
// from the repo root and want the spec where they ran the command.
func resolvePlanOutPath(out, configDir string) (string, error) {
	if out == "-" {
		return "-", nil
	}
	if out == "" {
		return filepath.Join(configDir, "kl-plan.json"), nil
	}
	abs, err := filepath.Abs(out)
	if err != nil {
		return "", fmt.Errorf("resolve --out: %w", err)
	}
	return abs, nil
}

// renderPlanSummary prints a short human-readable summary to w
// (typically stderr). The full spec goes to disk via --out; this is
// the at-a-glance recap operators see immediately. Format matches
// the refresh summary style for consistency.
func renderPlanSummary(w io.Writer, spec *plan.PlanSpec) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	defer tw.Flush()

	fmt.Fprintln(tw, "kl plan summary")
	fmt.Fprintf(tw, "  config:\t%s\n", spec.ConfigDir)
	fmt.Fprintf(tw, "  terraform:\t%s\n", spec.TerraformVersion)
	fmt.Fprintln(tw, "  actions:")
	fmt.Fprintf(tw, "    create:\t%d\n", spec.PlanSummary.Create)
	fmt.Fprintf(tw, "    update:\t%d\n", spec.PlanSummary.Update)
	fmt.Fprintf(tw, "    delete:\t%d\n", spec.PlanSummary.Delete)
	fmt.Fprintf(tw, "    replace:\t%d\n", spec.PlanSummary.Replace)
	fmt.Fprintf(tw, "    read:\t%d\n", spec.PlanSummary.Read)
	fmt.Fprintf(tw, "    no-op:\t%d\n", spec.PlanSummary.NoOp)
	fmt.Fprintf(tw, "    forget:\t%d\n", spec.PlanSummary.Forget)
	fmt.Fprintf(tw, "    total:\t%d\n", spec.PlanSummary.Total)
	mutatingActions := spec.PlanSummary.Create + spec.PlanSummary.Update + spec.PlanSummary.Delete + spec.PlanSummary.Replace + spec.PlanSummary.Forget

	fmt.Fprintf(tw, "  reservations:\t%d (%d write, %d read)\n",
		len(spec.Reservations),
		countReservations(spec.Reservations, "write"),
		countReservations(spec.Reservations, "read"))

	if n := len(spec.Variables); n > 0 {
		fmt.Fprintf(tw, "  variables (%d, pinned to spec):\n", n)
		keys := make([]string, 0, n)
		for k := range spec.Variables {
			keys = append(keys, k)
		}
		sortStrings(keys)
		for _, k := range keys {
			// RawMessage is JSON; print it verbatim so operators
			// see strings as `"v2"` and numbers/objects as their
			// HCL-shaped JSON forms.
			fmt.Fprintf(tw, "    %s=%s\n", k, string(spec.Variables[k]))
		}
	}

	if spec.StateName != "" {
		fmt.Fprintf(tw, "  state:\t%s\n", spec.StateName)
	}

	if n := len(spec.WriteSet); n > 0 {
		switch {
		case len(spec.ScopedFiles) > 0 && mutatingActions == 0:
			fmt.Fprintf(tw, "  scoped writable addresses (top %d, ownership only; plan is no-op):\n", min(n, maxAddressesShown))
		case len(spec.ScopedFiles) > 0:
			fmt.Fprintf(tw, "  scoped write set (top %d):\n", min(n, maxAddressesShown))
		default:
			fmt.Fprintf(tw, "  write set (top %d):\n", min(n, maxAddressesShown))
		}
		for i, a := range spec.WriteSet {
			if i >= maxAddressesShown {
				fmt.Fprintf(tw, "    ... and %d more\n", n-maxAddressesShown)
				break
			}
			fmt.Fprintf(tw, "    %s\n", a)
		}
	}
}

// maxAddressesShown caps the per-set address list in the CLI summary
// so a 10k-resource plan doesn't drown the terminal. The full list
// lives in the spec on disk; operators can `jq` for more.
const maxAddressesShown = 25

func countReservations(rs []plan.PlanReservation, mode string) int {
	n := 0
	for _, r := range rs {
		if r.Mode == mode {
			n++
		}
	}
	return n
}

func quotaPlanDeltaFromSummary(s plan.PlanSummary) int {
	return s.Create - s.Delete - s.Forget
}

// sortStrings sorts a slice in place. Tiny helper to avoid pulling
// in the entire `sort` import for one call.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
