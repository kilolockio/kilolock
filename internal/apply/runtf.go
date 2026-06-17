package apply

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/davesade/kilolock/internal/plan"
	"github.com/davesade/kilolock/internal/slice"
)

// runTerraform executes `terraform init` followed by
// `terraform apply -auto-approve -input=false -refresh=false` inside
// dir. Output from both commands is captured and surfaced on error
// so the operator sees what terraform actually said.
//
// stdout/stderr is also forwarded to logSink in real time when
// non-nil — typical CLI use passes os.Stderr so operators see
// progress without waiting for the run to finish.
//
// targets, when non-empty, are passed to terraform apply as
// `-target=<addr>` repeated for each entry. Terraform then prunes
// its graph to those addresses + their transitive dependencies,
// which limits the apply scope to that subgraph.
//
// Refresh-is-disabled rationale (the dominant perf win for v2c-1):
//
// The slice we hand terraform is, by construction, the latest
// authoritative content of the trunk for every resource that
// matters to this apply. We hold v2a reservations on the entire
// write set + read set; no other writer can have mutated any
// reservation member between our trunk fetch and our apply.
// Re-running terraform's refresh phase inside the tmp dir would
// re-call every provider's Read for every targeted resource +
// transitive deps purely to discover that nothing changed since
// we read the trunk a moment ago. That cost is the entire reason
// vanilla `terraform apply` is O(state) on large states.
//
// Disabling refresh trades this overhead for the operator's
// implicit assumption that "the trunk was fresh when I planned":
//   - if drift existed at plan time, the plan-spec already
//     reflects it (terraform's plan phase by default refreshes)
//   - if drift accrued between plan and apply, no refresh in
//     apply would catch it either — that race is the v2c-2
//     plan-staleness guard's job, not the apply tmp dir's.
//
// vars (when non-empty) become -var=NAME=VALUE arguments to
// terraform apply, in sorted order so the args list is
// deterministic. These are the operator-supplied variables that
// `kl plan` baked into the spec; replaying them here means
// the apply-time plan inside the tmp dir matches what the plan
// observed, even if the operator's shell env has drifted.
//
// terraformBin defaults to "terraform" on $PATH when empty.
func runTerraform(ctx context.Context, dir, terraformBin string, noColor bool, targets []string, vars map[string]json.RawMessage, pluginCacheDir string, logSink *bytes.Buffer) error {
	if dir == "" {
		return fmt.Errorf("runTerraform: dir is empty")
	}
	if terraformBin == "" {
		terraformBin = "terraform"
	}
	stream := io.Writer(nil)
	if logSink != nil {
		stream = io.MultiWriter(logSink, os.Stderr)
	}

	if stream != nil {
		fmt.Fprintln(os.Stderr, "kl apply: terraform init…")
	}
	if err := runOne(ctx, dir, terraformBin, []string{"init", "-input=false", noColorFlag(noColor)}, buildTerraformEnv(pluginCacheDir), stream, logSink); err != nil {
		return fmt.Errorf("terraform init: %w", err)
	}

	if stream != nil && len(targets) > 0 {
		fmt.Fprintln(os.Stderr, "kl apply: validating targeted replan…")
	}
	if err := validateReplanMatchesTargets(ctx, dir, terraformBin, targets, vars, pluginCacheDir); err != nil {
		return err
	}

	args := []string{"apply", "-auto-approve", "-input=false", "-refresh=false", noColorFlag(noColor)}
	for _, addr := range targets {
		if addr == "" {
			continue
		}
		args = append(args, "-target="+addr)
	}
	args = append(args, plan.TerraformVarArgs(vars)...)
	if stream != nil {
		fmt.Fprintln(os.Stderr, "kl apply: terraform apply…")
	}
	if err := runOne(ctx, dir, terraformBin, args, buildTerraformEnv(pluginCacheDir), stream, logSink); err != nil {
		return fmt.Errorf("terraform apply: %w", err)
	}
	return nil
}

// validateReplanMatchesTargets runs a terraform plan inside the apply
// workspace and asserts its write_set equals the predicted target set.
//
// This is a correctness guard against stale plan specs, variable drift,
// or any other mismatch between "what the operator planned" and what
// the apply workspace would actually change.
func validateReplanMatchesTargets(ctx context.Context, dir, terraformBin string, targets []string, vars map[string]json.RawMessage, pluginCacheDir string) error {
	// Empty targets means "full apply"; in that mode the check isn't
	// meaningful (it would require comparing full action lists) and is
	// intentionally skipped for now.
	if len(targets) == 0 {
		return nil
	}

	tfplan, err := os.CreateTemp(dir, ".kl-apply-replan-*.tfplan")
	if err != nil {
		return fmt.Errorf("replan: create tmp plan: %w", err)
	}
	tfplan.Close()
	defer os.Remove(tfplan.Name())

	fmt.Fprintf(os.Stderr, "kl apply: validation terraform plan (%d targets)…\n", len(targets))
	if err := runTerraformPlanStreaming(ctx, terraformBin, dir, tfplan.Name(), plan.PlanRunOptions{
		Lock:    false,
		Refresh: false,
		Vars:    vars,
		Targets: targets,
	}, pluginCacheDir); err != nil {
		return fmt.Errorf("replan: terraform plan: %w", err)
	}
	fmt.Fprintln(os.Stderr, "kl apply: validation terraform show…")
	b, err := plan.RunTerraformShow(ctx, terraformBin, dir, tfplan.Name())
	if err != nil {
		return fmt.Errorf("replan: terraform show: %w", err)
	}
	f, err := plan.ParseShowJSONBytes(b)
	if err != nil {
		return fmt.Errorf("replan: parse plan json: %w", err)
	}

	want := slice.IndexFootprintByGroup(targets)
	got := slice.IndexFootprintByGroup(plan.ExtractWriteSet(f))

	var missing, extra []string
	for a := range want {
		if _, ok := got[a]; !ok {
			missing = append(missing, a)
		}
	}
	for a := range got {
		if _, ok := want[a]; !ok {
			extra = append(extra, a)
		}
	}
	if len(missing) == 0 && len(extra) == 0 {
		return nil
	}
	sort.Strings(missing)
	sort.Strings(extra)
	return fmt.Errorf("replan validation failed: write_set mismatch (missing=%v extra=%v)", missing, extra)
}

func runTerraformPlanStreaming(ctx context.Context, terraformBin, workingDir, planFile string, opts plan.PlanRunOptions, pluginCacheDir string) error {
	if terraformBin == "" {
		terraformBin = "terraform"
	}
	args := []string{"plan", "-input=false", "-out=" + planFile}
	if !opts.Lock {
		args = append(args, "-lock=false")
	}
	if !opts.Refresh {
		args = append(args, "-refresh=false")
	}
	for _, target := range opts.Targets {
		if target == "" {
			continue
		}
		args = append(args, "-target="+target)
	}
	args = append(args, plan.TerraformVarArgs(opts.Vars)...)

	cmd := exec.CommandContext(ctx, terraformBin, args...)
	cmd.Dir = workingDir
	cmd.Env = buildTerraformEnv(pluginCacheDir)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %v: %w", terraformBin, args, err)
	}
	return nil
}

// noColorFlag returns "-no-color" or an empty string so it can be
// folded into an args slice unconditionally. terraform tolerates
// the empty entry as a no-op.
func noColorFlag(on bool) string {
	if on {
		return "-no-color"
	}
	return ""
}

// runOne invokes one terraform subcommand. Filters out empty
// argv entries (so the conditional -no-color flag doesn't leak as
// a literal "" argument). Always combines stdout+stderr because
// terraform interleaves them.
func runOne(ctx context.Context, dir, bin string, args []string, env []string, stream io.Writer, capture *bytes.Buffer) error {
	final := args[:0]
	for _, a := range args {
		if a != "" {
			final = append(final, a)
		}
	}
	cmd := exec.CommandContext(ctx, bin, final...)
	cmd.Dir = dir
	cmd.Env = env

	if stream != nil {
		cmd.Stdout = stream
		cmd.Stderr = stream
		if err := cmd.Run(); err != nil {
			if capture != nil {
				return fmt.Errorf("%s %v: %w\n%s", bin, final, err, capture.String())
			}
			return fmt.Errorf("%s %v: %w", bin, final, err)
		}
		return nil
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %v: %w\n%s", bin, final, err, string(out))
	}
	return nil
}

func buildTerraformEnv(pluginCacheDir string) []string {
	env := os.Environ()
	if strings.TrimSpace(pluginCacheDir) == "" {
		return env
	}
	out := make([]string, 0, len(env)+1)
	replaced := false
	for _, entry := range env {
		if strings.HasPrefix(entry, "TF_PLUGIN_CACHE_DIR=") {
			out = append(out, "TF_PLUGIN_CACHE_DIR="+pluginCacheDir)
			replaced = true
			continue
		}
		out = append(out, entry)
	}
	if !replaced {
		out = append(out, "TF_PLUGIN_CACHE_DIR="+pluginCacheDir)
	}
	return out
}

// readPostApplyState reads the terraform.tfstate file Terraform
// wrote during the apply. Returns the raw bytes plus the parsed
// shape; the caller usually wants both (the bytes for inspection /
// logging, the parsed form for the merge logic).
func readPostApplyState(dir string) ([]byte, error) {
	path := filepath.Join(dir, "terraform.tfstate")
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read post-apply state %s: %w", path, err)
	}
	return b, nil
}
