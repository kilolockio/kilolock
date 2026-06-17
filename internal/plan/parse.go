package plan

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

// ParseShowJSON decodes the byte stream produced by
// `terraform show -json plan.tfplan` into a File. Unknown JSON
// fields are silently ignored so future Terraform versions adding
// new top-level keys don't break us.
//
// Returns an error if the stream is not valid JSON or if the
// resulting File is structurally broken (missing format_version,
// which indicates the input is not actually a terraform-show-json
// output).
func ParseShowJSON(r io.Reader) (*File, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read plan json: %w", err)
	}
	return ParseShowJSONBytes(b)
}

// ParseShowJSONBytes is the byte-slice form of ParseShowJSON. Useful
// when the caller already has the JSON buffered (e.g. the typical
// runner path that calls RunTerraformShow first and then parses).
func ParseShowJSONBytes(b []byte) (*File, error) {
	var f File
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("decode plan json: %w", err)
	}
	if err := validate(&f); err != nil {
		return nil, err
	}
	return &f, nil
}

// validate enforces structural invariants the rest of internal/plan
// relies on. Surfaces fixture bugs early instead of letting a
// downstream nil-deref tell us about them.
func validate(f *File) error {
	if f.FormatVersion == "" {
		return fmt.Errorf("plan json: missing format_version (not a `terraform show -json` output?)")
	}
	// resource_changes can legitimately be empty in synthetic test
	// fixtures, so we don't reject an empty slice here. If a real
	// plan ends up with zero entries that's a Terraform bug the
	// orchestrator will detect downstream (write_set will be empty
	// and the apply will refuse to run with "nothing to do").
	return nil
}

// RunTerraformShow invokes `terraform show -json planFile` in the
// supplied working directory and returns the raw bytes. Callers
// typically immediately pass the result to ParseShowJSONBytes.
//
// terraformBin defaults to "terraform" when empty. The working
// directory must be the same one the plan was generated in (the
// plan file references the working dir's .terraform cache for
// provider metadata).
//
// Cancelable via ctx; the underlying exec.CommandContext will SIGKILL
// the terraform process when ctx is done.
func RunTerraformShow(ctx context.Context, terraformBin, workingDir, planFile string) ([]byte, error) {
	if terraformBin == "" {
		terraformBin = "terraform"
	}
	cmd := exec.CommandContext(ctx, terraformBin, "show", "-json", planFile)
	cmd.Dir = workingDir
	cmd.Env = os.Environ()
	if tfLogEnabled() {
		cmd.Stderr = os.Stderr
	}
	out, err := cmd.Output()
	if err != nil {
		// stderr carries the human-readable explanation
		// (unsupported plan format, missing providers, etc.);
		// surface it so the operator doesn't have to re-run with
		// -v to see what went wrong.
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("terraform show -json %s: %w\n%s",
				planFile, err, string(ee.Stderr))
		}
		return nil, fmt.Errorf("terraform show -json %s: %w", planFile, err)
	}
	return out, nil
}

// PlanRunOptions configures one invocation of `terraform plan`
// launched by kl. All fields are optional with safe defaults.
type PlanRunOptions struct {
	// Lock controls whether the http backend lock cycle is taken.
	// Default true. Operators with a long-running apply that already
	// holds row-level reservations can disable this so the plan
	// doesn't fight the backend's whole-state lock.
	Lock bool

	// Refresh controls terraform's per-resource refresh phase.
	//
	// Default true: terraform calls each provider's ReadResource for
	// every resource in state, catches drift, and the plan is
	// accurate against current reality. This is the dominant cost
	// for large states — O(state) provider calls.
	//
	// Set false to skip the refresh phase. The resulting plan
	// compares state-as-stored against the HCL only; drift in the
	// real world is not detected. This is the fast path for
	// iterative edits where the operator knows what they're
	// changing and trusts the trunk to be current — typical
	// turnaround drops from O(state) to a few seconds regardless
	// of state size. v2c-3's re-plan-inside-slice path will make
	// the safe (with-refresh) path fast too; until then -refresh=
	// false is the operator's escape hatch.
	Refresh bool

	// Vars is a map of terraform input variables to override during
	// the plan. Each value is JSON-encoded (so `"foo"` for a
	// string, `3` for a number, `{"env":"prod"}` for an object).
	// Each entry becomes a `-var=NAME=VALUE` argument to terraform;
	// terraform parses VALUE as HCL, and JSON is a subset of HCL
	// syntax for values so the encoding works uniformly across all
	// types.
	//
	// Iteration order is sorted so the args list is deterministic
	// (matters for shell history, audit logs, and the unit tests).
	Vars map[string]json.RawMessage

	// Targets limits terraform plan/apply to the named addresses.
	Targets []string
}

// DefaultPlanRunOptions are the safe defaults: lock taken, refresh
// performed. Matches what bare `terraform plan` would do.
func DefaultPlanRunOptions() PlanRunOptions {
	return PlanRunOptions{Lock: true, Refresh: true}
}

// terraformVarArgs renders Vars as a sorted slice of `-var=NAME=VALUE`
// strings ready to splice into a terraform args list. Sorted so the
// resulting command line is deterministic regardless of map iteration
// order — important for tests, audit logs, and shell history.
//
// Encoding rule, per Terraform's CLI semantics:
//
//   - Strings are passed UNQUOTED. Terraform's `-var=NAME=VALUE` flag
//     treats VALUE as a literal string for primitive types (string,
//     number, bool) — it does not parse VALUE as HCL. Splicing a
//     JSON-encoded string `"foo"` here would set the variable to the
//     4-character literal "foo" (with the quote characters embedded),
//     not the 3-character foo. This was a real, demo-breaking
//     regression: time_sleep.slow_a/slow_b triggers ended up with
//     `version = "v2"` (literal quotes inside the string) which then
//     poisoned downstream plans on every subsequent run.
//
//   - Numbers, booleans, objects, and arrays are passed as their
//     compacted JSON form. JSON is a subset of HCL syntax for these
//     shapes; Terraform parses the value back into the variable's
//     declared type. (Strictly speaking, complex types are best
//     passed via -var-file, but inline is fine for the simple cases
//     v0 actually emits.)
//
// Exported as a package-level helper so the apply orchestrator can
// reuse it when invoking terraform apply with the same variables.
func terraformVarArgs(vars map[string]json.RawMessage) []string {
	if len(vars) == 0 {
		return nil
	}
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, fmt.Sprintf("-var=%s=%s", k, renderVarValue(vars[k])))
	}
	return out
}

// renderVarValue renders one JSON-encoded variable value as the VALUE
// half of a Terraform `-var=NAME=VALUE` argument.
//
// JSON strings are unquoted: `"foo"` becomes `foo`. JSON null becomes
// the empty string (Terraform treats an empty -var= as null for
// nullable variables; this preserves the spec author's intent).
// Anything else is compacted into a single-line JSON form so the
// shell/exec layer keeps it as one argv token.
//
// Falls back to the raw bytes on a JSON-decode failure so the
// operator sees Terraform's parse error, not a silent truncation.
func renderVarValue(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	switch tv := v.(type) {
	case string:
		return tv
	case nil:
		return ""
	}
	// Numbers, bools, objects, arrays: compact-JSON-encode so the
	// argv token has no embedded newlines.
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return string(raw)
	}
	return compact.String()
}

// TerraformVarArgs is the exported form of terraformVarArgs so
// callers outside the package (notably internal/apply) can render
// the same `-var=NAME=VALUE` argument list without duplicating the
// sort + format logic.
func TerraformVarArgs(vars map[string]json.RawMessage) []string {
	return terraformVarArgs(vars)
}

// EncodeStringVar JSON-encodes a plain-string CLI value so it can
// be stored in the spec's Variables map and rendered as a valid
// `-var=NAME=VALUE` argument. We always go via strconv.Quote
// (instead of json.Marshal) so the encoding is fully predictable
// for tests and operator-readable diffs — strconv.Quote and
// json.Marshal produce identical output for valid UTF-8 strings,
// which is what every Terraform variable value is.
func EncodeStringVar(s string) json.RawMessage {
	return json.RawMessage(strconv.Quote(s))
}

// RunTerraformPlan invokes `terraform plan -out=planFile` in the
// supplied working directory, returning nil on success. The caller
// is expected to have already run `terraform init` (RunTerraformShow
// will refuse to read the resulting plan otherwise).
//
// `-input=false` suppresses any interactive prompts the providers
// might want to issue — this binary is non-interactive by design.
func RunTerraformPlan(ctx context.Context, terraformBin, workingDir, planFile string, opts PlanRunOptions) error {
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
	args = append(args, terraformVarArgs(opts.Vars)...)
	cmd := exec.CommandContext(ctx, terraformBin, args...)
	cmd.Dir = workingDir
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if tfLogEnabled() && len(out) > 0 {
		_, _ = os.Stderr.Write(out)
	}
	if err != nil {
		return fmt.Errorf("terraform plan -out=%s: %w\n%s", planFile, err, string(out))
	}
	return nil
}

// RunTerraformInit initializes a temporary planning/apply workspace.
// Used by ADR-0014 scoped planning before running terraform plan
// against a synthesized local-backend workspace.
func RunTerraformInit(ctx context.Context, terraformBin, workingDir string) error {
	if terraformBin == "" {
		terraformBin = "terraform"
	}
	cmd := exec.CommandContext(ctx, terraformBin, "init", "-input=false", "-reconfigure")
	cmd.Dir = workingDir
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if tfLogEnabled() && len(out) > 0 {
		_, _ = os.Stderr.Write(out)
	}
	if err != nil {
		return fmt.Errorf("terraform init: %w\n%s", err, string(out))
	}
	return nil
}

func tfLogEnabled() bool {
	v := strings.TrimSpace(os.Getenv("TF_LOG"))
	if v == "" {
		return false
	}
	return !strings.EqualFold(v, "off")
}
