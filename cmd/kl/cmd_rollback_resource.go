package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/davesade/kilolock/pkg/store"
)

func runRollbackResource(args []string) int {
	fs := flag.NewFlagSet("rollback resource", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var (
		address = fs.String("address", "", "Exact Terraform resource address to replay.")
		to      = fs.String("to", "", "Target version reference: serial, @N, uuid, or current.")
		apply   = fs.Bool("apply", false, "Actually perform the resource rollback.")
		format  = fs.String("format", "table", "Output format: table|json.")
		strict  = fs.Bool("strict", false, "Refuse risky rollback apply when live dependents exist or the replay would remove the current resource from state.")
		timeout = fs.Duration("timeout", resourceQueryTimeout, "Request timeout for preview/apply (e.g. 30s, 2m).")
		yes     = fs.Bool("yes", false, "Skip interactive confirmation.")
	)
	fs.BoolVar(yes, "y", false, "Alias for --yes.")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "kl rollback resource:", err)
		return 2
	}
	if fs.NArg() > 1 {
		fmt.Fprintln(os.Stderr, "kl rollback resource: too many positional arguments")
		return 2
	}
	if *address == "" || *to == "" {
		fmt.Fprintln(os.Stderr, "kl rollback resource: --address and --to are required")
		return 2
	}
	stateName, _, err := resolveStateName(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, "kl rollback resource:", err)
		return 2
	}
	client, err := newAPIClient()
	if err != nil {
		fmt.Fprintln(os.Stderr, "kl rollback resource:", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(cliContext(), *timeout)
	defer cancel()
	var preview store.ResourceRollbackPreview
	if err := client.postJSON(ctx, "/admin/state/resource-rollback/preview?name="+queryEscape(stateName), stateName, map[string]any{
		"address": *address,
		"to":      *to,
	}, &preview); err != nil {
		fmt.Fprintln(os.Stderr, "kl rollback resource:", err)
		return 1
	}
	switch *format {
	case "json":
		if err := renderResourceRollbackPreviewJSON(os.Stdout, &preview); err != nil {
			fmt.Fprintln(os.Stderr, "kl rollback resource:", err)
			return 1
		}
	case "table", "":
		renderResourceRollbackPreview(os.Stdout, &preview)
	default:
		fmt.Fprintln(os.Stderr, "kl rollback resource: --format must be table or json")
		return 2
	}
	if !*apply || preview.Action == "no-op" {
		if *format == "json" {
			return 0
		}
		if preview.Action == "no-op" {
			fmt.Fprintln(os.Stdout, "No-op: current state already matches the selected historical resource.")
		} else {
			fmt.Fprintln(os.Stdout, "Dry-run only. Re-run with --apply to write a new state version.")
		}
		return 0
	}
	if err := enforceResourceRollbackStrict(*strict, &preview); err != nil {
		fmt.Fprintln(os.Stderr, "kl rollback resource:", err)
		return 2
	}
	if !*yes && !confirmResourceRollback(os.Stdin, os.Stdout, *address) {
		fmt.Fprintln(os.Stderr, "kl rollback resource: cancelled by operator")
		return 2
	}
	var resp struct {
		OK      bool                          `json:"ok"`
		Preview store.ResourceRollbackPreview `json:"preview"`
		Version *store.StateVersionInfo       `json:"version"`
	}
	if err := client.postJSON(ctx, "/admin/state/resource-rollback/apply?name="+queryEscape(stateName), stateName, map[string]any{
		"address": *address,
		"to":      *to,
		"actor":   cliActor(),
	}, &resp); err != nil {
		fmt.Fprintln(os.Stderr, "kl rollback resource:", err)
		return 1
	}
	if resp.Version == nil {
		fmt.Fprintln(os.Stderr, "kl rollback resource: backend did not return new version metadata")
		return 1
	}
	if *format == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		if err := enc.Encode(resp); err != nil {
			fmt.Fprintln(os.Stderr, "kl rollback resource:", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(os.Stdout, "\nResource rollback applied.\n  new serial: %d\n  new version id: %s\n", resp.Version.Serial, resp.Version.ID)
	return 0
}

func enforceResourceRollbackStrict(strict bool, preview *store.ResourceRollbackPreview) error {
	if !strict || preview == nil {
		return nil
	}
	var violations []string
	if len(preview.Dependents) > 0 {
		violations = append(violations, fmt.Sprintf("%d current dependent resource(s): %s", len(preview.Dependents), summarizeAddresses(preview.Dependents, 3)))
	}
	if preview.Action == "remove" {
		violations = append(violations, "selected historical version does not contain the address, so the rollback would remove the current resource from state")
	}
	if len(violations) == 0 {
		return nil
	}
	return fmt.Errorf("strict rollback rejected apply: %s", strings.Join(violations, "; "))
}

func summarizeAddresses(addresses []string, limit int) string {
	if len(addresses) == 0 {
		return ""
	}
	if limit <= 0 || len(addresses) <= limit {
		return strings.Join(addresses, ", ")
	}
	return fmt.Sprintf("%s, ...", strings.Join(addresses[:limit], ", "))
}
