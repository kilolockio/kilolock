package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/davesade/kilolock/internal/refresh"
	"github.com/davesade/kilolock/pkg/store"
)

// runRefresh is the entrypoint for `kl refresh <state-name>`.
//
// Exit codes:
//
//	0 - run succeeded; zero per-resource failures
//	1 - run produced per-resource failures, OR run-level error
//	    (state not found, DB unreachable, etc.); see stderr for
//	    details
//	2 - argv / usage error
//
// The terminal status is what the audit row reflects too — exit 0
// implies the new state_version was written (unless --dry-run).
func runRefresh(args []string) int {
	fs := flag.NewFlagSet("refresh", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // we render usage ourselves on parse error

	var (
		failFast    = fs.Bool("fail-fast", false, "Stop on first per-resource error.")
		concurrency = fs.Int("concurrency", 0, "Max parallel provider groups (0 = NumCPU).")
		dryRun      = fs.Bool("dry-run", false, "Compute drift but do not write a new state version.")
		actor       = fs.String("actor", "", "Actor string for the audit log (default: derived from $USER).")
	)
	var searchPaths multiString
	fs.Var(&searchPaths, "provider-search-path", "Provider binary search path (repeatable; or set KL_PROVIDER_PATH=p1:p2).")

	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "kl refresh:", err)
		fmt.Fprint(os.Stderr, refreshUsage)
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "kl refresh: missing required <state-name> argument")
		fmt.Fprint(os.Stderr, refreshUsage)
		return 2
	}
	if fs.NArg() > 1 {
		fmt.Fprintf(os.Stderr, "kl refresh: unexpected extra arguments: %v\n", fs.Args()[1:])
		return 2
	}
	stateName := fs.Arg(0)

	resolved, err := resolveSearchPaths(searchPaths, os.Getenv("KL_PROVIDER_PATH"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "kl refresh:", err)
		return 2
	}

	sigCtx, sigCancel := signal.NotifyContext(cliContext(), os.Interrupt, syscall.SIGTERM)
	defer sigCancel()
	ctx, cancel := context.WithTimeout(sigCtx, refreshTimeout)
	defer cancel()
	client, err := newAPIClient()
	if err != nil {
		fmt.Fprintln(os.Stderr, "kl refresh:", err)
		return 1
	}

	opts := refresh.Options{
		StateName:   stateName,
		Concurrency: effectiveConcurrency(*concurrency),
		FailFast:    *failFast,
		DryRun:      *dryRun,
		Actor:       firstNonEmpty(*actor, cliActor()),
	}
	var resp struct {
		RunError string `json:"run_error"`
		Result   *struct {
			RunID            string `json:"run_id"`
			StateName        string `json:"state_name"`
			Status           string `json:"status"`
			SerialBefore     int64  `json:"serial_before"`
			SerialAfter      int64  `json:"serial_after"`
			ResourcesChecked int    `json:"resources_checked"`
			ResourcesChanged int    `json:"resources_changed"`
			ResourcesFailed  int    `json:"resources_failed"`
			Errors           []struct {
				Address string `json:"address"`
				Error   string `json:"error"`
			} `json:"errors"`
			StartedAt        time.Time `json:"started_at"`
			FinishedAt       time.Time `json:"finished_at"`
			DryRun           bool      `json:"dry_run"`
			ChangedAddresses []string  `json:"changed_addresses"`
		} `json:"result"`
	}
	if err := client.postJSON(ctx, "/admin/state/refresh", stateName, map[string]any{
		"state_name":   opts.StateName,
		"concurrency":  opts.Concurrency,
		"fail_fast":    opts.FailFast,
		"dry_run":      opts.DryRun,
		"actor":        opts.Actor,
		"search_paths": resolved,
	}, &resp); err != nil {
		fmt.Fprintln(os.Stderr, "kl refresh:", err)
		return 1
	}
	if resp.Result == nil {
		fmt.Fprintln(os.Stderr, "kl refresh: backend returned no result")
		return 1
	}
	res := &refresh.Result{
		RunID:            resp.Result.RunID,
		StateName:        resp.Result.StateName,
		Status:           store.RefreshRunStatus(resp.Result.Status),
		SerialBefore:     resp.Result.SerialBefore,
		SerialAfter:      resp.Result.SerialAfter,
		ResourcesChecked: resp.Result.ResourcesChecked,
		ResourcesChanged: resp.Result.ResourcesChanged,
		ResourcesFailed:  resp.Result.ResourcesFailed,
		StartedAt:        resp.Result.StartedAt,
		FinishedAt:       resp.Result.FinishedAt,
		DryRun:           resp.Result.DryRun,
		ChangedAddresses: resp.Result.ChangedAddresses,
	}
	for _, item := range resp.Result.Errors {
		res.Errors = append(res.Errors, refresh.ResourceError{
			Address: item.Address,
			Err:     errors.New(item.Error),
		})
	}
	var runErr error
	if strings.TrimSpace(resp.RunError) != "" {
		runErr = errors.New(resp.RunError)
	}

	renderRefreshResult(os.Stdout, res, runErr)

	switch res.Status {
	case store.RefreshRunSucceeded:
		return 0
	default:
		return 1
	}
}

const refreshUsage = `Usage:
  kl refresh <state-name> [flags]

Flags:
  --fail-fast                  Stop on first per-resource error.
  --concurrency=N              Max parallel provider groups (default: NumCPU).
  --dry-run                    Compute drift but do not write a new state version.
  --actor=NAME                 Actor string for the audit log.
  --provider-search-path=DIR   Provider binary search path. Repeatable; falls
                               back to KL_PROVIDER_PATH (colon-separated)
                               and then a default set including ~/.terraform.d/
                               plugin-cache, ./.terraform/providers, and
                               TF_PLUGIN_CACHE_DIR if set.

Exit status:
  0  succeeded — every resource refreshed; new state version written
  1  failed    — at least one resource failed, or a run-level error
  2  argv      — usage error
`

// refreshTimeout is the per-invocation deadline. Set generously: a
// real-world AWS state with 10k resources at ~50ms per ReadResource
// (network latency dominated) is ~8 minutes serially. With provider
// parallelism that drops to a couple of minutes, but credential
// expiry (assume_role with 15-minute STS session) caps how long
// any single Run can usefully take. 30 minutes is the safe upper
// bound; operators with longer-running refreshes should split by
// state.
const refreshTimeout = 30 * time.Minute

// multiString is a flag.Value that accumulates repeated string flags.
// The standard library has no built-in repeatable-string flag; rolling
// our own is cheaper than depending on a third-party flag library.
type multiString []string

func (m *multiString) String() string {
	if m == nil {
		return ""
	}
	return strings.Join(*m, ",")
}
func (m *multiString) Set(v string) error {
	*m = append(*m, v)
	return nil
}

// resolveSearchPaths assembles the final search-path list applied to
// provider.Discover. The precedence is, highest to lowest:
//
//  1. --provider-search-path flags (each instance, in argv order)
//  2. KL_PROVIDER_PATH env (colon-separated)
//  3. Built-in defaults: ~/.terraform.d/plugin-cache,
//     ./.terraform/providers, $TF_PLUGIN_CACHE_DIR
//
// Empty entries are skipped silently. Non-existent paths are kept;
// Discover walks them in order and tolerates per-path readdir errors,
// so a stale entry doesn't block discovery from a later path.
//
// Returns an error only when no candidate paths exist after
// expansion — that's a usage-level mistake (no flags, no env, no
// home dir, no .terraform/providers directory) the operator needs
// to be told about explicitly.
func resolveSearchPaths(flagPaths []string, envPath string) ([]string, error) {
	var out []string

	out = append(out, normalizePaths(flagPaths)...)

	if envPath != "" {
		out = append(out, normalizePaths(strings.Split(envPath, string(os.PathListSeparator)))...)
	}

	out = append(out, defaultSearchPaths()...)

	out = dedupePaths(out)

	if len(out) == 0 {
		return nil, errors.New("no provider search paths configured (set --provider-search-path or KL_PROVIDER_PATH)")
	}
	return out, nil
}

func normalizePaths(in []string) []string {
	out := make([]string, 0, len(in))
	for _, p := range in {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		// Best-effort absolute-path resolution. Discover accepts
		// relative paths fine, but absolute paths produce more
		// useful error messages on failure.
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		out = append(out, p)
	}
	return out
}

func defaultSearchPaths() []string {
	var out []string
	if home, err := os.UserHomeDir(); err == nil {
		out = append(out, filepath.Join(home, ".terraform.d", "plugin-cache"))
	}
	if cwd, err := os.Getwd(); err == nil {
		out = append(out, filepath.Join(cwd, ".terraform", "providers"))
	}
	if cache := os.Getenv("TF_PLUGIN_CACHE_DIR"); cache != "" {
		out = append(out, cache)
	}
	return out
}

func dedupePaths(in []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, p := range in {
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}

func effectiveConcurrency(n int) int {
	if n > 0 {
		return n
	}
	return runtime.NumCPU()
}

func firstNonEmpty(parts ...string) string {
	for _, p := range parts {
		if p != "" {
			return p
		}
	}
	return ""
}

// renderRefreshResult writes the operator-facing summary to w. The
// shape is intentionally a key/value table rather than free prose so
// it stays grep-friendly and stable across runs (drift detection on
// the output itself, e.g. via diff of two refresh runs, becomes
// trivial). runErr is non-nil when the orchestrator finished but
// returned an error (Finish failed, etc.); it is summarized in a
// "warnings" trailer rather than swallowed.
func renderRefreshResult(w io.Writer, res *refresh.Result, runErr error) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	defer tw.Flush()

	fmt.Fprintf(tw, "state\t%s\n", res.StateName)
	fmt.Fprintf(tw, "run id\t%s\n", res.RunID)
	fmt.Fprintf(tw, "serial\t%s\n", formatSerial(res))
	fmt.Fprintf(tw, "checked\t%d\n", res.ResourcesChecked)
	fmt.Fprintf(tw, "changed\t%d\n", res.ResourcesChanged)
	fmt.Fprintf(tw, "failed\t%d\n", res.ResourcesFailed)
	if !res.FinishedAt.IsZero() && !res.StartedAt.IsZero() {
		fmt.Fprintf(tw, "duration\t%s\n", res.FinishedAt.Sub(res.StartedAt).Round(time.Millisecond))
	}
	fmt.Fprintf(tw, "status\t%s\n", res.Status)
	tw.Flush()

	if len(res.ChangedAddresses) > 0 {
		fmt.Fprintln(w, "drift addresses:")
		// res.ChangedAddresses is already sorted at the
		// orchestrator boundary; iterate verbatim so the
		// output is stable across runs.
		for _, addr := range truncateAddrList(res.ChangedAddresses, maxDriftAddressesShown) {
			fmt.Fprintf(w, "  %s\n", addr)
		}
		if extra := len(res.ChangedAddresses) - maxDriftAddressesShown; extra > 0 {
			fmt.Fprintf(w, "  ... and %d more (query current_resource_drift for the full list)\n", extra)
		}
	}

	if len(res.Errors) > 0 {
		fmt.Fprintln(w, "errors:")
		for _, e := range res.Errors {
			fmt.Fprintf(w, "  %s\n", e.Error())
		}
	}
	if runErr != nil {
		fmt.Fprintf(w, "warnings: %v\n", runErr)
	}
}

// maxDriftAddressesShown caps the per-run drift list in the CLI
// summary so a 10k-resource pathological refresh doesn't drown the
// terminal. The full list lives in the database (state_versions for
// the new version, current_resource_drift for the query view).
const maxDriftAddressesShown = 25

// truncateAddrList returns the first n addresses, or the whole
// slice when len(addrs) <= n. Caller adds the "... and N more"
// footer if it cares about the elided count.
func truncateAddrList(addrs []string, n int) []string {
	if len(addrs) <= n {
		return addrs
	}
	return addrs[:n]
}

// formatSerial renders the before→after transition in a way that
// distinguishes commit, dry-run, and failure paths at a glance.
func formatSerial(res *refresh.Result) string {
	switch {
	case res.DryRun:
		return fmt.Sprintf("%d (dry-run; no version written)", res.SerialBefore)
	case res.SerialAfter == res.SerialBefore:
		return fmt.Sprintf("%d (no version written; refresh failed)", res.SerialBefore)
	default:
		return fmt.Sprintf("%d → %d (committed)", res.SerialBefore, res.SerialAfter)
	}
}
