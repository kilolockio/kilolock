package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/kilolockio/kilolock/pkg/store"
)

// runRollback implements `kl rollback`.
//
// Conceptually: read a historical state_versions row, write its
// raw_state back at MAX(serial)+1 with source='rollback' and an
// audit-log entry. Atomic, append-only — the previous "current"
// version stays in the history, recoverable by another rollback.
//
// The most-likely-misused feature of this whole project sits in
// this file, so the UX is deliberately careful:
//
//  1. The default is --dry-run (you must opt in with --apply
//     to actually write). This is the opposite of the usual
//     `kl` convention, where commands write by default;
//     here we trade an extra keystroke for a smaller chance of
//     wrecking somebody's production state.
//  2. --apply alone still prompts for an interactive confirmation
//     ("type the state name to confirm"); --yes/-y skips the
//     prompt for CI.
//  3. The dry-run output prominently includes the "rolling back
//     state ≠ rolling back infrastructure" warning, because that
//     is the single most likely operator mistake.
//
// Exit codes:
//
//	0  rollback applied (or dry-run completed)
//	1  database / orchestration error
//	2  argv / state-not-found / cancelled-by-operator
func runRollback(args []string) int {
	if len(args) > 0 && strings.TrimSpace(args[0]) == "resource" {
		return runRollbackResource(args[1:])
	}
	fs := flag.NewFlagSet("rollback", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var (
		to    = fs.String("to", "", "Target version reference: a serial (e.g. 42), a relative ref (@1 = previous, @2 = two back, …), or a state_version UUID.")
		apply = fs.Bool("apply", false, "Actually perform the rollback. Without this flag, the command runs in dry-run mode and only prints what would happen.")
		yes   = fs.Bool("yes", false, "Skip the interactive confirmation prompt (use in CI). Ignored in dry-run mode.")
		actor = fs.String("actor", "", "Override the actor recorded on the new state_version (default: $USER@cli).")
	)
	fs.BoolVar(yes, "y", false, "Alias for --yes.")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "kl rollback:", err)
		fmt.Fprint(os.Stderr, rollbackUsage)
		return 2
	}
	if fs.NArg() > 1 {
		fmt.Fprintf(os.Stderr, "kl rollback: too many positional arguments: %v\n", fs.Args())
		fmt.Fprint(os.Stderr, rollbackUsage)
		return 2
	}
	if *to == "" {
		fmt.Fprintln(os.Stderr, "kl rollback: --to=<serial|@N|uuid> is required")
		fmt.Fprint(os.Stderr, rollbackUsage)
		return 2
	}

	stateName, _, err := resolveStateName(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, "kl rollback:", err)
		fmt.Fprint(os.Stderr, rollbackUsage)
		return 2
	}

	ctx, cancel := context.WithTimeout(cliContext(), 5*time.Minute)
	defer cancel()
	client, err := newAPIClient()
	if err != nil {
		fmt.Fprintln(os.Stderr, "kl rollback:", err)
		return 1
	}

	// Resolve the current head + the rollback target up-front so the
	// dry-run output is fully populated without any writes.
	var preview struct {
		State   string                    `json:"state"`
		Current *store.StateVersionInfo   `json:"current"`
		Target  *store.StateVersionInfo   `json:"target"`
		Diff    *store.VersionAddressDiff `json:"diff"`
	}
	if err := client.postJSON(ctx, "/admin/state/rollback/preview?name="+queryEscape(stateName), stateName, map[string]any{"to": *to}, &preview); err != nil {
		if strings.Contains(err.Error(), "state not found") || strings.Contains(err.Error(), "target version not found") || strings.Contains(err.Error(), "target resolves to the current version") {
			fmt.Fprintln(os.Stderr, "kl rollback:", err)
			return 2
		}
		fmt.Fprintln(os.Stderr, "kl rollback:", err)
		return 1
	}
	current, target, diff := preview.Current, preview.Target, preview.Diff
	if current == nil || target == nil || diff == nil {
		fmt.Fprintln(os.Stderr, "kl rollback: backend returned incomplete preview")
		return 1
	}
	if target.ID == current.ID {
		fmt.Fprintf(os.Stderr, "kl rollback: target %q resolves to the current version (serial %d); nothing to do\n", *to, current.Serial)
		return 2
	}

	renderRollbackPreview(os.Stdout, stateName, current, target, diff)

	if !*apply {
		fmt.Fprintln(os.Stdout)
		fmt.Fprintln(os.Stdout, "Dry-run only. Re-run with --apply to perform the rollback.")
		return 0
	}

	if !*yes {
		if !confirmRollback(os.Stdin, os.Stdout, stateName) {
			fmt.Fprintln(os.Stderr, "kl rollback: cancelled by operator")
			return 2
		}
	}

	resolvedActor := *actor
	if resolvedActor == "" {
		resolvedActor = cliActor()
	}

	var applyResp struct {
		OK      bool                    `json:"ok"`
		Version *store.StateVersionInfo `json:"version"`
	}
	if err := client.postJSON(ctx, "/admin/state/rollback/apply?name="+queryEscape(stateName), stateName, map[string]any{
		"to":    *to,
		"actor": resolvedActor,
	}, &applyResp); err != nil {
		fmt.Fprintln(os.Stderr, "kl rollback:", err)
		return 1
	}
	newVersion := applyResp.Version
	if newVersion == nil {
		fmt.Fprintln(os.Stderr, "kl rollback: backend did not return new version metadata")
		return 1
	}

	fmt.Fprintln(os.Stdout)
	fmt.Fprintln(os.Stdout, "Rollback applied.")
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "  new serial:\t%d\n", newVersion.Serial)
	fmt.Fprintf(tw, "  new version id:\t%s\n", newVersion.ID)
	fmt.Fprintf(tw, "  source version:\tserial %d (%s)\n", target.Serial, shortUUID(target.ID))
	fmt.Fprintf(tw, "  actor:\t%s\n", newVersion.CreatedBy)
	_ = tw.Flush()
	fmt.Fprintln(os.Stdout)
	fmt.Fprintln(os.Stdout, "Reminder: this rolled the state (bookkeeping) only.")
	fmt.Fprintln(os.Stdout, "Run `terraform plan` to see how the cloud reality diverges from the rolled state,")
	fmt.Fprintln(os.Stdout, "and revert the HCL that introduced the rolled-back changes before `terraform apply`.")
	return 0
}

const rollbackUsage = `Usage:
  kl rollback [state] --to=<ref> [flags]
  kl rollback resource [state] --address=<addr> --to=<ref> [flags]

Re-issues the raw_state of a past state_version as a new version at
` + "`MAX(serial)+1`" + `. The previous current version is preserved in
` + "`state_versions`" + ` — every rollback is itself just another append.

DRY-RUN BY DEFAULT. The command will show what would change and
exit 0 without writing. Pass --apply to perform the rollback.

Positional:
  state                 State name (default: auto-detected from the
                        http backend address of the CWD).

Required:
  --to=<ref>            Target version. Accepted shapes:
                          <serial>       e.g. --to=42
                          @<N>           N versions back (@1 = previous,
                                         @2 = two back, …)
                          <uuid>         a state_versions.id

Flags:
  --apply               Perform the rollback (otherwise: dry-run only).
  --strict              Resource rollback only: refuse apply when live
                        dependents exist or when the replay would remove
                        the current resource from state.
  --timeout=DURATION    Resource rollback only: request timeout for preview
                        and apply (default: 2m).
  --yes, -y             Skip interactive confirmation (CI mode).
  --actor=NAME          Override the actor recorded in state_versions.
                        Default: $USER@cli.

WARNING:
  Rolling back the state rewinds kl's bookkeeping; it does
  NOT roll back any cloud resources. Resources that were created
  AFTER the target version will become unmanaged orphans unless the
  HCL that introduced them is also reverted before the next
  ` + "`terraform apply`" + `. Read the dry-run output carefully.

Exit status:
  0  rollback applied (or dry-run completed)
  1  database or orchestration error
  2  usage / state-not-found / cancelled by operator
`

// renderRollbackPreview prints the operator-facing preview of a
// rollback. Used in both dry-run mode and as the lead-in to the
// confirmation prompt of an --apply.
//
// The output deliberately leads with the warning that rolling
// back state is not rolling back infrastructure, because that is
// the single point operators get wrong every time and the support
// ticket from hell that follows is the reason this whole command
// exists with --dry-run as the default.
func renderRollbackPreview(w io.Writer, stateName string, current, target *store.StateVersionInfo, diff *store.VersionAddressDiff) {
	fmt.Fprintf(w, "Rollback preview for state %q:\n\n", stateName)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "  current serial:\t%d  (%s)\n", current.Serial, shortUUID(current.ID))
	fmt.Fprintf(tw, "  current actor:\t%s\n", emptyDash(current.CreatedBy))
	fmt.Fprintf(tw, "  current written:\t%s (UTC)\n", current.CreatedAt.UTC().Format("2006-01-02 15:04:05"))
	fmt.Fprintln(tw)
	fmt.Fprintf(tw, "  target serial:\t%d  (%s)\n", target.Serial, shortUUID(target.ID))
	fmt.Fprintf(tw, "  target actor:\t%s\n", emptyDash(target.CreatedBy))
	fmt.Fprintf(tw, "  target written:\t%s (UTC)\n", target.CreatedAt.UTC().Format("2006-01-02 15:04:05"))
	fmt.Fprintf(tw, "  target source:\t%s\n", emptyDash(target.Source))
	_ = tw.Flush()

	fmt.Fprintln(w)
	fmt.Fprintln(w, "Address-level changes if applied:")
	renderAddressList(w, "  +", "added to state (was missing in current; present in target)", diff.Added)
	renderAddressList(w, "  -", "removed from state (present in current; missing in target)", diff.Removed)
	renderAddressList(w, "  ~", "attributes change (present in both, attrs differ)", diff.Changed)
	if len(diff.Added)+len(diff.Removed)+len(diff.Changed) == 0 {
		fmt.Fprintln(w, "  (no resource-level differences; only metadata)")
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "WARNING: this rolls back the state (bookkeeping) only.")
	if len(diff.Removed) > 0 {
		fmt.Fprintf(w, "  → %d resource(s) will become unmanaged orphans in the cloud\n", len(diff.Removed))
		fmt.Fprintln(w, "    unless the HCL that introduced them is also reverted before the next `terraform apply`.")
	}
	if len(diff.Added) > 0 {
		fmt.Fprintf(w, "  → %d resource(s) currently absent will reappear in state\n", len(diff.Added))
		fmt.Fprintln(w, "    if they don't exist in the cloud, the next `terraform apply` will try to recreate them.")
	}
}

// renderAddressList prints a sorted-by-caller list of addresses
// with a leading marker and (above a threshold) a "...and N more"
// truncation. Operators looking at a rollback preview need a sense
// of magnitude more than they need the full list.
func renderAddressList(w io.Writer, marker, caption string, addrs []string) {
	if len(addrs) == 0 {
		return
	}
	const max = 20
	fmt.Fprintf(w, "%s %s [%d]:\n", marker, caption, len(addrs))
	for i, a := range addrs {
		if i >= max {
			fmt.Fprintf(w, "      ... and %d more\n", len(addrs)-max)
			break
		}
		fmt.Fprintf(w, "      %s\n", a)
	}
}

// confirmRollback asks the operator to type the state name back to
// confirm. We deliberately do NOT accept "y" / "yes" / "Y" alone —
// rolling back is destructive enough that the operator should be
// forced to look at what they're about to do.
func confirmRollback(stdin io.Reader, stdout io.Writer, stateName string) bool {
	fmt.Fprintln(stdout)
	fmt.Fprintf(stdout, "Type %q to confirm the rollback (anything else cancels): ", stateName)
	sc := bufio.NewScanner(stdin)
	if !sc.Scan() {
		return false
	}
	return strings.TrimSpace(sc.Text()) == stateName
}
