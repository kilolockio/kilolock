package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/kilolockio/kilolock/pkg/store"
)

func runQueryResources(args []string) int {
	fs := flag.NewFlagSet("query resources", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var (
		addressGlob = fs.String("address-glob", "", "Optional glob filter for live resource addresses.")
		limit       = fs.Int("limit", 200, "Max number of resources to return.")
		format      = fs.String("format", "table", "Output format: table|json.")
		timeout     = fs.Duration("timeout", resourceQueryTimeout, "Request timeout (e.g. 30s, 2m).")
	)
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "kl query resources:", err)
		return 2
	}
	if fs.NArg() > 1 {
		fmt.Fprintln(os.Stderr, "kl query resources: too many positional arguments")
		return 2
	}
	stateName, _, err := resolveStateName(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, "kl query resources:", err)
		return 2
	}
	client, err := newAPIClient()
	if err != nil {
		fmt.Fprintln(os.Stderr, "kl query resources:", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(cliContext(), *timeout)
	defer cancel()
	var resp struct {
		State     string                   `json:"state"`
		Resources []store.ResourceSnapshot `json:"resources"`
	}
	path := fmt.Sprintf("/admin/state/resources?name=%s&address_glob=%s&limit=%d", queryEscape(stateName), queryEscape(*addressGlob), *limit)
	if err := client.getJSON(ctx, path, &resp); err != nil {
		fmt.Fprintln(os.Stderr, "kl query resources:", err)
		return 1
	}
	switch *format {
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(resp); err != nil {
			fmt.Fprintln(os.Stderr, "kl query resources:", err)
			return 1
		}
		return 0
	case "table", "":
		fmt.Fprintf(os.Stdout, "state: %s\n\n", resp.State)
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "ADDRESS\tTYPE\tMODE\tPROVIDER\tCREATED")
		for _, row := range resp.Resources {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\n", row.Address, row.Type, row.Mode, row.Provider, row.CreateSerial)
		}
		_ = tw.Flush()
		fmt.Fprintf(os.Stdout, "\n(%d resource%s)\n", len(resp.Resources), plural(len(resp.Resources)))
		return 0
	default:
		fmt.Fprintln(os.Stderr, "kl query resources: --format must be table or json")
		return 2
	}
}

func runQueryResource(args []string) int {
	fs := flag.NewFlagSet("query resource", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var (
		address = fs.String("address", "", "Exact Terraform resource address to inspect.")
		format  = fs.String("format", "table", "Output format: table|json.")
		timeout = fs.Duration("timeout", resourceQueryTimeout, "Request timeout (e.g. 30s, 2m).")
	)
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "kl query resource:", err)
		return 2
	}
	if fs.NArg() > 1 {
		fmt.Fprintln(os.Stderr, "kl query resource: too many positional arguments")
		return 2
	}
	if *address == "" {
		fmt.Fprintln(os.Stderr, "kl query resource: --address is required")
		return 2
	}
	stateName, _, err := resolveStateName(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, "kl query resource:", err)
		return 2
	}
	client, err := newAPIClient()
	if err != nil {
		fmt.Fprintln(os.Stderr, "kl query resource:", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(cliContext(), *timeout)
	defer cancel()
	var resp struct {
		State    string                  `json:"state"`
		Resource *store.ResourceSnapshot `json:"resource"`
	}
	path := fmt.Sprintf("/admin/state/resource?name=%s&address=%s", queryEscape(stateName), queryEscape(*address))
	if err := client.getJSON(ctx, path, &resp); err != nil {
		fmt.Fprintln(os.Stderr, "kl query resource:", err)
		return 1
	}
	if resp.Resource == nil {
		fmt.Fprintln(os.Stderr, "kl query resource: backend returned no resource")
		return 1
	}
	switch *format {
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(resp); err != nil {
			fmt.Fprintln(os.Stderr, "kl query resource:", err)
			return 1
		}
		return 0
	case "table", "":
		row := resp.Resource
		fmt.Fprintf(os.Stdout, "state:   %s\naddress: %s\n\n", resp.State, row.Address)
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintf(tw, "  type:\t%s\n", row.Type)
		fmt.Fprintf(tw, "  mode:\t%s\n", row.Mode)
		fmt.Fprintf(tw, "  provider:\t%s\n", row.Provider)
		fmt.Fprintf(tw, "  module:\t%s\n", emptyDash(row.ModulePath))
		fmt.Fprintf(tw, "  created serial:\t%d\n", row.CreateSerial)
		if row.IndexKind != "" && row.IndexKind != "none" {
			fmt.Fprintf(tw, "  index:\t%s=%s\n", row.IndexKind, row.IndexValue)
		}
		_ = tw.Flush()
		if len(row.Attributes) > 0 {
			fmt.Fprintln(os.Stdout)
			fmt.Fprintln(os.Stdout, "attributes:")
			var pretty any
			if err := json.Unmarshal(row.Attributes, &pretty); err == nil {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("  ", "  ")
				_ = enc.Encode(pretty)
			} else {
				fmt.Fprintf(os.Stdout, "  %s\n", string(row.Attributes))
			}
		}
		return 0
	default:
		fmt.Fprintln(os.Stderr, "kl query resource: --format must be table or json")
		return 2
	}
}

func runQueryResourceHistory(args []string) int {
	fs := flag.NewFlagSet("query history", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var (
		address = fs.String("address", "", "Exact Terraform resource address to inspect.")
		limit   = fs.Int("limit", 50, "Max number of history spans to return.")
		format  = fs.String("format", "table", "Output format: table|json.")
		timeout = fs.Duration("timeout", resourceQueryTimeout, "Request timeout (e.g. 30s, 2m).")
	)
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "kl query history:", err)
		return 2
	}
	if fs.NArg() > 1 {
		fmt.Fprintln(os.Stderr, "kl query history: too many positional arguments")
		return 2
	}
	if *address == "" {
		fmt.Fprintln(os.Stderr, "kl query history: --address is required")
		return 2
	}
	stateName, _, err := resolveStateName(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, "kl query history:", err)
		return 2
	}
	client, err := newAPIClient()
	if err != nil {
		fmt.Fprintln(os.Stderr, "kl query history:", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(cliContext(), *timeout)
	defer cancel()
	var resp struct {
		State   string                       `json:"state"`
		Address string                       `json:"address"`
		History []store.ResourceHistoryEntry `json:"history"`
	}
	path := fmt.Sprintf("/admin/state/resource-history?name=%s&address=%s&limit=%d", queryEscape(stateName), queryEscape(*address), *limit)
	if err := client.getJSON(ctx, path, &resp); err != nil {
		fmt.Fprintln(os.Stderr, "kl query history:", err)
		return 1
	}
	switch *format {
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(resp); err != nil {
			fmt.Fprintln(os.Stderr, "kl query history:", err)
			return 1
		}
		return 0
	case "table", "":
		fmt.Fprintf(os.Stdout, "state:   %s\naddress: %s\n\n", resp.State, resp.Address)
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "STATUS\tCREATED\tDELETED\tTYPE\tSOURCE")
		for _, row := range resp.History {
			deleted := "open"
			if row.DeleteSerial != nil {
				deleted = fmt.Sprintf("%d", *row.DeleteSerial)
			}
			fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\n",
				describeResourceHistoryRow(row),
				row.CreateSerial,
				deleted,
				row.Type,
				emptyDash(row.CreateVersionSrc),
			)
		}
		_ = tw.Flush()
		fmt.Fprintf(os.Stdout, "\n(%d history row%s)\n", len(resp.History), plural(len(resp.History)))
		return 0
	default:
		fmt.Fprintln(os.Stderr, "kl query history: --format must be table or json")
		return 2
	}
}

func queryEscape(s string) string {
	return url.QueryEscape(s)
}

func describeResourceHistoryRow(row store.ResourceHistoryEntry) string {
	switch {
	case strings.HasPrefix(row.CreateVersionSrc, "resource-rollback:"):
		if row.DeleteSerial == nil {
			return "restored-current"
		}
		return "restored-old"
	case row.DeleteSerial == nil:
		return "current"
	default:
		return "superseded"
	}
}

func renderResourceRollbackPreview(w io.Writer, preview *store.ResourceRollbackPreview) {
	fmt.Fprintf(w, "Resource rollback preview\n\n")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "  state:\t%s\n", preview.StateName)
	fmt.Fprintf(tw, "  address:\t%s\n", preview.Address)
	fmt.Fprintf(tw, "  action:\t%s\n", preview.Action)
	fmt.Fprintf(tw, "  current:\tserial %d (%s, %s)\n", preview.CurrentVersion.Serial, shortUUID(preview.CurrentVersion.ID), emptyDash(preview.CurrentVersion.Source))
	fmt.Fprintf(tw, "  target:\tserial %d (%s, %s)\n", preview.TargetVersion.Serial, shortUUID(preview.TargetVersion.ID), emptyDash(preview.TargetVersion.Source))
	fmt.Fprintf(tw, "  current exists:\t%t\n", preview.CurrentExists)
	fmt.Fprintf(tw, "  target exists:\t%t\n", preview.TargetExists)
	_ = tw.Flush()
	if len(preview.Dependencies) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Dependencies:")
		for _, dep := range preview.Dependencies {
			fmt.Fprintf(w, "  - %s\n", dep)
		}
	}
	if len(preview.Dependents) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Dependents in current state:")
		for _, dep := range preview.Dependents {
			fmt.Fprintf(w, "  - %s\n", dep)
		}
	}
	if len(preview.Warnings) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Warnings:")
		for _, warning := range preview.Warnings {
			fmt.Fprintf(w, "  - %s\n", warning)
		}
	}
	renderResourceAttributeDiff(w, preview)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "WARNING: this repairs state bookkeeping only.")
	fmt.Fprintln(w, "Run `terraform plan` afterwards to inspect cloud/state divergence.")
}

func renderResourceAttributeDiff(w io.Writer, preview *store.ResourceRollbackPreview) {
	grouped, err := buildResourceRollbackDiff(preview)
	if err != nil {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "Attribute changes: (failed to parse attributes: %v)\n", err)
		return
	}
	fmt.Fprintln(w)
	if grouped.Total() == 0 {
		fmt.Fprintln(w, "Attribute changes: none")
		return
	}
	fmt.Fprintln(w, "Attribute changes:")
	renderResourceDiffGroup(w, "Changed", grouped.Changed, 20)
	renderResourceDiffGroup(w, "Added", grouped.Added, 20)
	renderResourceDiffGroup(w, "Removed", grouped.Removed, 20)
}

func mergePreviewSensitive(preview *store.ResourceRollbackPreview) pathSet {
	a := newPathSet(preview.CurrentSens)
	b := newPathSet(preview.TargetSens)
	for k := range b {
		a[k] = struct{}{}
	}
	return a
}

type resourceRollbackDiff struct {
	Added   []jsonPathLeaf
	Removed []jsonPathLeaf
	Changed []jsonPathLeaf
}

func (d resourceRollbackDiff) Total() int {
	return len(d.Added) + len(d.Removed) + len(d.Changed)
}

func buildResourceRollbackDiff(preview *store.ResourceRollbackPreview) (resourceRollbackDiff, error) {
	leaves, err := diffJSON(preview.CurrentAttrs, preview.TargetAttrs, mergePreviewSensitive(preview))
	if err != nil {
		return resourceRollbackDiff{}, err
	}
	var out resourceRollbackDiff
	for _, leaf := range leaves {
		switch leaf.Status {
		case "added":
			out.Added = append(out.Added, leaf)
		case "removed":
			out.Removed = append(out.Removed, leaf)
		case "changed":
			out.Changed = append(out.Changed, leaf)
		}
	}
	return out, nil
}

func renderResourceDiffGroup(w io.Writer, title string, leaves []jsonPathLeaf, max int) {
	if len(leaves) == 0 {
		return
	}
	fmt.Fprintf(w, "  %s:\n", title)
	renderLeavesSample(w, leaves, max)
}

type jsonRollbackLeaf struct {
	Path      string `json:"path"`
	Status    string `json:"status"`
	Before    any    `json:"before,omitempty"`
	After     any    `json:"after,omitempty"`
	Sensitive bool   `json:"sensitive,omitempty"`
}

type jsonRollbackPreview struct {
	Preview    *store.ResourceRollbackPreview `json:"preview"`
	Added      []jsonRollbackLeaf             `json:"added,omitempty"`
	Removed    []jsonRollbackLeaf             `json:"removed,omitempty"`
	Changed    []jsonRollbackLeaf             `json:"changed,omitempty"`
	Total      int                            `json:"total"`
	WarningMsg string                         `json:"warning"`
}

func renderResourceRollbackPreviewJSON(w io.Writer, preview *store.ResourceRollbackPreview) error {
	grouped, err := buildResourceRollbackDiff(preview)
	if err != nil {
		return err
	}
	out := jsonRollbackPreview{
		Preview:    preview,
		Added:      toJSONRollbackLeaves(grouped.Added),
		Removed:    toJSONRollbackLeaves(grouped.Removed),
		Changed:    toJSONRollbackLeaves(grouped.Changed),
		Total:      grouped.Total(),
		WarningMsg: "state bookkeeping only; run terraform plan afterwards",
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(out)
}

func toJSONRollbackLeaves(in []jsonPathLeaf) []jsonRollbackLeaf {
	out := make([]jsonRollbackLeaf, 0, len(in))
	for _, leaf := range in {
		out = append(out, jsonRollbackLeaf{
			Path:      leaf.Path,
			Status:    leaf.Status,
			Before:    redactedOrValue(leaf.Before, leaf.Sensitive),
			After:     redactedOrValue(leaf.After, leaf.Sensitive),
			Sensitive: leaf.Sensitive,
		})
	}
	return out
}

func confirmResourceRollback(r io.Reader, w io.Writer, address string) bool {
	fmt.Fprintf(w, "Type the full resource address to confirm rollback of %q: ", address)
	var typed string
	_, _ = fmt.Fscanln(r, &typed)
	return typed == address
}
