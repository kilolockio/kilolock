package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/davesade/kilolock/internal/plan"
	"github.com/davesade/kilolock/pkg/store"
)

// runHistory drives `kl history`. Prints recent state_versions
// rows for one state, newest first, so operators can pick a target
// for `kl rollback`.
//
// Exit codes:
//
//	0  printed at least one row
//	1  state exists but has no versions (impossible today; defensive)
//	2  argv / state-not-found / DB error
//
// State-name resolution mirrors `plan`/`apply`: a positional argument
// wins; otherwise we fall back to DiscoverBackend against CWD; if
// neither resolves we abort with a usage error.
func runHistory(args []string) int {
	fs := flag.NewFlagSet("history", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var (
		limit  = fs.Int("limit", 20, "Max number of versions to show (newest first); 0 for unlimited.")
		offset = fs.Int("offset", 0, "Skip the N most-recent versions (for pagination).")
		format = fs.String("format", "table", "Output format: table|json")
	)
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "kl history:", err)
		fmt.Fprint(os.Stderr, historyUsage)
		return 2
	}
	if fs.NArg() > 1 {
		fmt.Fprintf(os.Stderr, "kl history: too many positional arguments: %v\n", fs.Args())
		fmt.Fprint(os.Stderr, historyUsage)
		return 2
	}

	stateName, _, err := resolveStateName(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, "kl history:", err)
		fmt.Fprint(os.Stderr, historyUsage)
		return 2
	}

	ctx, cancel := context.WithTimeout(cliContext(), defaultTimeout)
	defer cancel()
	client, err := newAPIClient()
	if err != nil {
		fmt.Fprintln(os.Stderr, "kl history:", err)
		return 1
	}
	var resp struct {
		State    string                   `json:"state"`
		Versions []store.StateVersionInfo `json:"versions"`
	}
	path := fmt.Sprintf("/admin/states/%s/history?limit=%d&offset=%d", stateName, *limit, *offset)
	if err := client.getJSON(ctx, path, &resp); err != nil {
		fmt.Fprintln(os.Stderr, "kl history:", err)
		return 1
	}
	versions := resp.Versions
	tagsByVersion := map[string][]string{}

	switch *format {
	case "json":
		return renderHistoryJSON(os.Stdout, stateName, versions, tagsByVersion)
	case "table", "":
		return renderHistoryTable(os.Stdout, stateName, versions, tagsByVersion, time.Now())
	default:
		fmt.Fprintf(os.Stderr, "kl history: --format must be table or json\n")
		return 2
	}
}

const historyUsage = `Usage:
  kl history [state] [flags]

Lists state versions newest-first. Each version is one row of the
` + "`state_versions`" + ` append-only history; nothing here ever overwrites
or deletes a previous version. Use the resulting ` + "`serial`" + ` or version
UUID as the --to= argument to ` + "`kl rollback`" + `.

Positional:
  state                 State name (default: auto-detected from the
                        http backend address of the CWD).

Flags:
  --limit=N             Max rows to show (default 20; 0 for unlimited).
  --offset=N            Skip the N most-recent versions (pagination).
  --format=FMT          table | json (default table).

Exit status:
  0  one or more versions printed
  1  database error
  2  usage error or state not found
`

// renderHistoryTable produces the human-readable form. Columns:
//
//   - marker for the row pointed to by states.current_version_id
//     serial   monotonic counter, the right value to pass to --to=
//     source   apply|refresh|import|rollback|unknown
//     when     relative + UTC absolute timestamp
//     actor    who triggered the write
//     size     state_versions.raw_state byte length
//     id       state_version uuid (truncated to first 8 chars)
//
// We deliberately don't show the version UUID in full: it's noisy
// and operators reason about `serial` 99% of the time. The truncated
// form is enough to disambiguate when serial-collision is impossible.
func renderHistoryTable(w io.Writer, stateName string, versions []store.StateVersionInfo, tagsByVersion map[string][]string, now time.Time) int {
	if len(versions) == 0 {
		fmt.Fprintf(w, "kl history: state %q has no versions yet\n", stateName)
		return 1
	}
	hasTags := false
	for _, ts := range tagsByVersion {
		if len(ts) > 0 {
			hasTags = true
			break
		}
	}
	fmt.Fprintf(w, "state: %s\n\n", stateName)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if hasTags {
		fmt.Fprintln(tw, "  \tSERIAL\tSOURCE\tWHEN\tACTOR\tSIZE\tID\tTAGS")
	} else {
		fmt.Fprintln(tw, "  \tSERIAL\tSOURCE\tWHEN\tACTOR\tSIZE\tID")
	}
	for _, v := range versions {
		marker := " "
		if v.IsCurrent {
			marker = "*"
		}
		base := fmt.Sprintf("%s\t%d\t%s\t%s (%s)\t%s\t%s\t%s",
			marker,
			v.Serial,
			emptyDash(v.Source),
			humanAge(now.Sub(v.CreatedAt)),
			v.CreatedAt.UTC().Format("2006-01-02 15:04:05"),
			emptyDash(v.CreatedBy),
			humanSize(int64(v.SizeBytes)),
			shortUUID(v.ID),
		)
		if hasTags {
			fmt.Fprintf(tw, "%s\t%s\n", base, emptyDash(strings.Join(tagsByVersion[v.ID], ", ")))
		} else {
			fmt.Fprintln(tw, base)
		}
	}
	_ = tw.Flush()
	fmt.Fprintln(w)
	fmt.Fprintln(w, "* = current version pointed to by states.current_version_id")
	fmt.Fprintln(w, "Use `kl rollback "+stateName+" --to=<serial>` to revert to a past version.")
	if hasTags {
		fmt.Fprintln(w, "Tag names (TAGS column) are accepted as <serial> in --to=, --from=, etc.")
	}
	return 0
}

// renderHistoryJSON emits the same data as the table form, in a
// stable shape suitable for piping into scripts. Field names mirror
// the column headings the table uses so the table → json transition
// is mechanical for operators.
func renderHistoryJSON(w io.Writer, stateName string, versions []store.StateVersionInfo, tagsByVersion map[string][]string) int {
	type row struct {
		Serial           int64    `json:"serial"`
		Source           string   `json:"source"`
		Actor            string   `json:"actor,omitempty"`
		CreatedAt        string   `json:"created_at"`
		SizeBytes        int      `json:"size_bytes"`
		ID               string   `json:"id"`
		TerraformVersion string   `json:"terraform_version,omitempty"`
		IsCurrent        bool     `json:"is_current"`
		Tags             []string `json:"tags,omitempty"`
	}
	out := struct {
		State    string `json:"state"`
		Versions []row  `json:"versions"`
	}{
		State:    stateName,
		Versions: make([]row, 0, len(versions)),
	}
	for _, v := range versions {
		out.Versions = append(out.Versions, row{
			Serial:           v.Serial,
			Source:           v.Source,
			Actor:            v.CreatedBy,
			CreatedAt:        v.CreatedAt.UTC().Format(time.RFC3339),
			SizeBytes:        v.SizeBytes,
			ID:               v.ID,
			TerraformVersion: v.TerraformVersion,
			IsCurrent:        v.IsCurrent,
			Tags:             tagsByVersion[v.ID],
		})
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		fmt.Fprintln(os.Stderr, "kl history:", err)
		return 1
	}
	return 0
}

// resolveStateName implements the engineer-friendly default:
// positional argument wins; otherwise discover from the http
// backend address of the CWD; otherwise return a clear error so
// the user knows to either `cd` into a module or pass the name.
// The bool return signals whether the name came from auto-discovery
// (true) — callers print a courtesy line so the operator sees what
// got picked.
func resolveStateName(positional string) (name string, discovered bool, err error) {
	if positional != "" {
		return positional, false, nil
	}
	bi, err := plan.DiscoverBackend(".")
	if err != nil {
		return "", false, fmt.Errorf("--state name required (no positional argument and no http backend discovered in CWD: %v)", err)
	}
	return bi.StateName, true, nil
}

// ---------------------------------------------------------------------------
// formatting helpers
// ---------------------------------------------------------------------------

// humanAge renders a duration as a short relative-time string. We
// stop at hours/minutes for recent versions and switch to days /
// weeks for older. Approximate by design — the operator already has
// the absolute UTC timestamp on the same row.
func humanAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return fmt.Sprintf("%dw ago", int(d.Hours()/(24*7)))
	}
}

// humanSize renders bytes in a kilo/mega form. Decimal multiples
// (1 KB = 1000 B) match what every other CLI tool produces; we
// don't second-guess that convention.
func humanSize(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%dB", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1fKB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1fMB", float64(n)/(1024*1024))
	}
}

// shortUUID returns the first 8 chars of a uuid (or the input
// unmodified for shorter strings). Enough to disambiguate visually
// in a 20-row table.
func shortUUID(s string) string {
	if len(s) <= 8 {
		return s
	}
	return s[:8]
}

func emptyDash(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "-"
	}
	return s
}
