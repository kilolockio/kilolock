package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/kilolockio/kilolock/internal/plan"
	"github.com/kilolockio/kilolock/pkg/store"
)

const quotaUsage = `Usage: kl quota <subcommand> [flags]

Subcommands:
  remaining [config-dir]   Show current quota headroom for the backend state.
  check [config-dir]       Check whether a Terraform plan would exceed quota.

remaining flags:
  --state NAME             State name. Defaults to the HTTP backend state in [config-dir].

check flags:
  --state NAME             State name. Defaults to the HTTP backend state in [config-dir].
  --tf-plan PATH           Terraform binary plan file (` + "`terraform plan -out=...`" + `).
  --tf-plan-json PATH      JSON from ` + "`terraform show -json`" + ` for a Terraform plan. Use - for stdin.
  --terraform-bin PATH     terraform binary path for --tf-plan. Default: "terraform" on $PATH.

Exit codes:
  0  within hard quota (soft overages print WARN)
  1  hard quota exceeded, backend check failed, or plan read failed
  2  argv / usage error
`

func runQuota(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, quotaUsage)
		return 2
	}
	switch args[0] {
	case "remaining":
		return runQuotaRemaining(args[1:])
	case "check":
		return runQuotaCheck(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "kl quota: unknown subcommand %q\n", args[0])
		fmt.Fprint(os.Stderr, quotaUsage)
		return 2
	}
}

func runQuotaRemaining(args []string) int {
	fs := flag.NewFlagSet("quota remaining", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	stateFlag := fs.String("state", "", "State name. Defaults to the discovered HTTP backend state.")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "kl quota remaining:", err)
		fmt.Fprint(os.Stderr, quotaUsage)
		return 2
	}
	if fs.NArg() > 1 {
		fmt.Fprintf(os.Stderr, "kl quota remaining: unexpected extra arguments: %v\n", fs.Args()[1:])
		return 2
	}
	configDir := "."
	if fs.NArg() == 1 {
		configDir = fs.Arg(0)
	}
	stateName, client, err := quotaClientAndState(configDir, *stateFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "kl quota remaining:", err)
		return 1
	}
	preview, err := client.getQuotaRemaining(cliContext(), stateName)
	if err != nil {
		fmt.Fprintln(os.Stderr, "kl quota remaining:", err)
		return 1
	}
	renderQuotaPreview(os.Stdout, preview)
	return 0
}

func runQuotaCheck(args []string) int {
	fs := flag.NewFlagSet("quota check", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	stateFlag := fs.String("state", "", "State name. Defaults to the discovered HTTP backend state.")
	tfPlan := fs.String("tf-plan", "", "Terraform binary plan file (`terraform plan -out=...`).")
	tfPlanJSON := fs.String("tf-plan-json", "", "JSON from `terraform show -json`. Use - for stdin.")
	terraformBin := fs.String("terraform-bin", "", `terraform binary path (default "terraform" on $PATH).`)
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "kl quota check:", err)
		fmt.Fprint(os.Stderr, quotaUsage)
		return 2
	}
	if fs.NArg() > 1 {
		fmt.Fprintf(os.Stderr, "kl quota check: unexpected extra arguments: %v\n", fs.Args()[1:])
		return 2
	}
	if strings.TrimSpace(*tfPlan) == "" && strings.TrimSpace(*tfPlanJSON) == "" {
		fmt.Fprintln(os.Stderr, "kl quota check: one of --tf-plan or --tf-plan-json is required")
		return 2
	}
	if strings.TrimSpace(*tfPlan) != "" && strings.TrimSpace(*tfPlanJSON) != "" {
		fmt.Fprintln(os.Stderr, "kl quota check: use either --tf-plan or --tf-plan-json, not both")
		return 2
	}
	configDir := "."
	if fs.NArg() == 1 {
		configDir = fs.Arg(0)
	}
	absConfigDir, err := filepath.Abs(configDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "kl quota check: resolve config dir: %v\n", err)
		return 2
	}
	stateName, client, err := quotaClientAndState(absConfigDir, *stateFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "kl quota check:", err)
		return 1
	}
	delta, err := quotaPlanDeltaFromFlags(cliContext(), absConfigDir, strings.TrimSpace(*terraformBin), strings.TrimSpace(*tfPlan), strings.TrimSpace(*tfPlanJSON))
	if err != nil {
		fmt.Fprintln(os.Stderr, "kl quota check:", err)
		return 1
	}
	preview, err := client.checkQuota(cliContext(), stateName, delta)
	if err != nil {
		fmt.Fprintln(os.Stderr, "kl quota check:", err)
		return 1
	}
	renderQuotaPreview(os.Stdout, preview)
	return quotaPreviewExitCode(os.Stderr, preview, "kl quota check")
}

func quotaClientAndState(configDir, stateFlag string) (string, *apiClient, error) {
	stateName := strings.TrimSpace(stateFlag)
	if stateName == "" {
		bi, err := plan.DiscoverBackend(configDir)
		if err != nil {
			return "", nil, fmt.Errorf("discover backend state: %w", err)
		}
		stateName = strings.TrimSpace(bi.StateName)
	}
	if stateName == "" {
		return "", nil, fmt.Errorf("state name is required")
	}
	client, err := newAPIClientFromBackend(configDir)
	if err != nil {
		return "", nil, err
	}
	return stateName, client, nil
}

func quotaPlanDeltaFromFlags(ctx context.Context, configDir, terraformBin, tfPlanPath, tfPlanJSONPath string) (int, error) {
	var raw []byte
	var err error
	switch {
	case tfPlanPath != "":
		raw, err = plan.RunTerraformShow(ctx, terraformBin, configDir, tfPlanPath)
	case tfPlanJSONPath == "-":
		raw, err = io.ReadAll(os.Stdin)
	default:
		raw, err = os.ReadFile(tfPlanJSONPath)
	}
	if err != nil {
		return 0, err
	}
	parsed, err := plan.ParseShowJSONBytes(raw)
	if err != nil {
		return 0, err
	}
	return managedResourcePlanDelta(parsed), nil
}

func managedResourcePlanDelta(f *plan.File) int {
	if f == nil {
		return 0
	}
	delta := 0
	for _, rc := range f.ResourceChanges {
		if strings.TrimSpace(rc.Mode) != "managed" {
			continue
		}
		switch plan.ClassifyChange(rc.Change) {
		case plan.ActionCreate:
			delta++
		case plan.ActionDelete, plan.ActionForget:
			delta--
		}
	}
	return delta
}

func quotaPreviewExitCode(w io.Writer, preview *store.QuotaPreview, prefix string) int {
	if preview == nil {
		return 0
	}
	if preview.State.HardExceeded || preview.Environment.HardExceeded {
		fmt.Fprintf(w, "%s: quota hard limit exceeded\n", prefix)
		return 1
	}
	if preview.State.SoftExceeded || preview.Environment.SoftExceeded {
		fmt.Fprintf(w, "%s: WARN quota soft limit exceeded\n", prefix)
	}
	return 0
}

func renderQuotaPreview(w io.Writer, preview *store.QuotaPreview) {
	if preview == nil {
		fmt.Fprintln(w, "no quota preview available")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	defer tw.Flush()
	fmt.Fprintln(tw, "kl quota")
	fmt.Fprintf(tw, "  tenant:\t%s\n", firstNonEmpty(preview.TenantSlug, preview.TenantID))
	fmt.Fprintf(tw, "  plan:\t%s\n", preview.BillingPlan)
	fmt.Fprintf(tw, "  state:\t%s\n", preview.StateName)
	renderQuotaDimension(tw, "state resources", preview.State)
	renderQuotaDimension(tw, "environment resources", preview.Environment)
}

func renderQuotaDimension(w *tabwriter.Writer, label string, d store.QuotaDimensionPreview) {
	if d.Unlimited {
		fmt.Fprintf(w, "  %s:\tcurrent=%d projected=%d delta=%+d unlimited\n", label, d.Current, d.Projected, d.PlannedDelta)
		return
	}
	status := "ok"
	switch {
	case d.HardExceeded:
		status = "HARD_EXCEEDED"
	case d.SoftExceeded:
		status = "SOFT_EXCEEDED"
	}
	fmt.Fprintf(w, "  %s:\tcurrent=%d projected=%d delta=%+d soft=%d hard=%d remaining_soft=%d remaining_hard=%d status=%s\n",
		label, d.Current, d.Projected, d.PlannedDelta, d.SoftLimit, d.HardLimit, d.RemainingSoft, d.RemainingHard, status)
}

func (c *apiClient) getQuotaRemaining(ctx context.Context, stateName string) (*store.QuotaPreview, error) {
	var out store.QuotaPreview
	if err := c.getJSON(ctx, "/admin/quota/remaining?state_name="+url.QueryEscape(stateName), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *apiClient) checkQuota(ctx context.Context, stateName string, plannedDelta int) (*store.QuotaPreview, error) {
	var out store.QuotaPreview
	in := map[string]any{
		"state_name":             stateName,
		"planned_resource_delta": plannedDelta,
	}
	if err := c.postJSON(ctx, "/admin/quota/check", stateName, in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
