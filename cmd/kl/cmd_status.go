package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/kilolockio/kilolock/pkg/store"
)

// runStatus drives `kl status [state]`. One-screen summary
// of a state's live operational situation: current version, active
// lock (if any), in-flight applies, held reservations.
//
// The command exists to answer the operator's standing question
// "is my apply hanging, or just slow?" without making them write
// SQL. Three failure modes are surfaced as distinct sections so
// the operator's eyes don't have to triage which signal to read
// first:
//
//   - LOCK held: vanilla terraform is holding the v1 state lock.
//     If this is a stale lock from a SIGKILL'd terraform process,
//     the recovery move is documented in the output.
//
//   - RESERVATIONS held by another apply: parallel apply v2 is
//     waiting for someone else's row-level lock. Names the holder,
//     the apply run, and the lease expiry. Nothing for the
//     operator to do besides wait, unless the holder is itself
//     stuck (then `status` on the holder's state tells them).
//
//   - In-flight apply rows: gives the operator the apply_run uuid
//     they need for `kl apply abort <id>` (forthcoming) or
//     to query the runs table directly.
//
// Exit codes:
//
//	0  state exists; status (possibly all-clear) printed
//	1  database error
//	2  argv error / state-not-found
func runStatus(args []string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	format := fs.String("format", "table", "Output format: table|json")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "kl status:", err)
		fmt.Fprint(os.Stderr, statusUsage)
		return 2
	}
	if fs.NArg() > 1 {
		fmt.Fprintf(os.Stderr, "kl status: too many positional arguments: %v\n", fs.Args())
		fmt.Fprint(os.Stderr, statusUsage)
		return 2
	}

	stateName, _, err := resolveStateName(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, "kl status:", err)
		fmt.Fprint(os.Stderr, statusUsage)
		return 2
	}

	ctx, cancel := context.WithTimeout(cliContext(), defaultTimeout)
	defer cancel()
	client, err := newAPIClient()
	if err != nil {
		fmt.Fprintln(os.Stderr, "kl status:", err)
		return 1
	}
	var status store.StateStatus
	if err := client.getJSON(ctx, "/admin/states/"+stateName+"/status", &status); err != nil {
		fmt.Fprintln(os.Stderr, "kl status:", err)
		return 1
	}

	switch *format {
	case "json":
		return renderStatusJSON(os.Stdout, &status)
	case "table", "":
		return renderStatusTable(os.Stdout, &status)
	default:
		fmt.Fprintf(os.Stderr, "kl status: unknown --format %q\n", *format)
		return 2
	}
}

// renderStatusTable writes the human-readable form. Sections are
// ordered by the question they answer: identity first ("did I get
// the right state?"), then escalating-severity blockers ("what's
// preventing me from making progress?").
func renderStatusTable(w io.Writer, st *store.StateStatus) int {
	now := time.Now().UTC()
	locks := st.Locks
	if len(locks) == 0 && st.Lock != nil {
		locks = append(locks, *st.Lock)
	}

	// Header: state identity + current version.
	fmt.Fprintf(w, "STATE  %s\n", st.Name)
	if st.Lineage != "" {
		fmt.Fprintf(w, "  lineage           %s\n", st.Lineage)
	}
	fmt.Fprintf(w, "  current serial    %d\n", st.CurrentSerial)
	if st.ExclusiveLocks {
		fmt.Fprintf(w, "  lock mode         exclusive (vanilla terraform serializes)\n")
	} else {
		fmt.Fprintf(w, "  lock mode         optimistic (vanilla terraform may run concurrently)\n")
	}
	if st.CoexistenceMode == "" {
		st.CoexistenceMode = store.StateCoexistenceWarn
	}
	fmt.Fprintf(w, "  coexistence mode  %s\n", st.CoexistenceMode)
	if st.TerraformVersion != "" {
		fmt.Fprintf(w, "  terraform version %s\n", st.TerraformVersion)
	}
	fmt.Fprintf(w, "  resources         %d alive\n", st.ResourceCount)
	fmt.Fprintf(w, "  updated           %s (%s)\n",
		humanAge(now.Sub(st.UpdatedAt.UTC())),
		st.UpdatedAt.UTC().Format("2006-01-02 15:04:05"))

	// Lock section. Visually distinguished so a held lock is
	// impossible to miss in a long output.
	fmt.Fprintln(w)
	if len(locks) == 0 {
		fmt.Fprintf(w, "LOCK  none\n")
	} else if len(locks) == 1 {
		lk := locks[0]
		fmt.Fprintf(w, "LOCK  HELD  (v1 HTTP backend whole-state lock)\n")
		fmt.Fprintf(w, "  lock id    %s\n", lk.LockID)
		if lk.Who != "" {
			fmt.Fprintf(w, "  who        %s\n", lk.Who)
		}
		if lk.Info != "" {
			fmt.Fprintf(w, "  info       %s\n", lk.Info)
		}
		fmt.Fprintf(w, "  acquired   %s (%s)\n",
			humanAge(now.Sub(lk.Created.UTC())),
			lk.Created.UTC().Format("2006-01-02 15:04:05"))
		fmt.Fprintf(w, "\n")
		fmt.Fprintf(w, "  Note: this is the v1 HTTP backend's whole-state lock,\n")
		fmt.Fprintf(w, "  used by vanilla terraform. The v2 `kl apply`\n")
		fmt.Fprintf(w, "  bypasses this lock and uses row-level reservations\n")
		fmt.Fprintf(w, "  (see RESERVATIONS section).\n")
		fmt.Fprintf(w, "  If this is a stale lock from a killed terraform run,\n")
		fmt.Fprintf(w, "  recover with:\n")
		fmt.Fprintf(w, "    psql \"$KL_DATABASE_URL\" -c \\\n")
		fmt.Fprintf(w, "      \"DELETE FROM state_locks WHERE state_id = \\\n")
		fmt.Fprintf(w, "         (SELECT id FROM states WHERE name = '%s');\"\n", st.Name)
	} else {
		fmt.Fprintf(w, "LOCKS  %d held  (v1 HTTP backend whole-state locks; optimistic mode)\n", len(locks))
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  LOCK ID\tWHO\tACQUIRED\tINFO")
		for _, lk := range locks {
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n",
				shortUUID(lk.LockID),
				orDash(lk.Who),
				humanAge(now.Sub(lk.Created.UTC())),
				orDash(lk.Info),
			)
		}
		tw.Flush()
		fmt.Fprintf(w, "\n")
		fmt.Fprintf(w, "  Note: multiple vanilla terraform clients currently hold\n")
		fmt.Fprintf(w, "  optimistic whole-state locks on this state. The backend\n")
		fmt.Fprintf(w, "  will reconcile disjoint write sets at commit time.\n")
	}

	if len(locks) > 0 && (len(st.InFlightApplies) > 0 || len(st.ActiveReservations) > 0) {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "COEXISTENCE WARNING  mixed vanilla terraform + kl apply activity detected\n")
		fmt.Fprintf(w, "  v1 whole-state locks and v2 row-level reservations are both active.\n")
		fmt.Fprintf(w, "  This is safe but operationally advanced: prefer one toolchain per state,\n")
		fmt.Fprintf(w, "  or switch vanilla terraform to exclusive mode if you want serialization.\n")
	}

	// In-flight apply runs. Three numbers (planned/applied/failed)
	// are exactly what an operator needs to gauge progress.
	fmt.Fprintln(w)
	if len(st.InFlightApplies) == 0 {
		fmt.Fprintf(w, "IN-FLIGHT APPLIES  none\n")
	} else {
		fmt.Fprintf(w, "IN-FLIGHT APPLIES  %d running\n", len(st.InFlightApplies))
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  APPLY ID\tACTOR\tFROM SERIAL\tSTARTED\tPLANNED\tAPPLIED\tFAILED")
		for _, ar := range st.InFlightApplies {
			fmt.Fprintf(tw, "  %s\t%s\t%d\t%s\t%s\t%s\t%s\n",
				shortUUID(ar.ID),
				orDash(ar.Actor),
				ar.SourceSerial,
				humanAge(now.Sub(ar.StartedAt.UTC())),
				countOrDash(ar.ResourcesPlanned),
				countOrDash(ar.ResourcesApplied),
				countOrDash(ar.ResourcesFailed),
			)
		}
		tw.Flush()
	}

	// Reservations. Group by apply id so the operator can see at a
	// glance "alice's apply holds X addresses; bob's apply holds Y".
	fmt.Fprintln(w)
	if len(st.ActiveReservations) == 0 {
		fmt.Fprintf(w, "RESERVATIONS  none held\n")
	} else {
		fmt.Fprintf(w, "RESERVATIONS  %d held\n", len(st.ActiveReservations))
		grouped := groupReservationsByApply(st.ActiveReservations)
		for _, g := range grouped {
			fmt.Fprintf(w, "  apply %s  (holder=%s, %d reservations, lease expires in %s)\n",
				shortUUID(g.applyID),
				g.holder,
				len(g.reservations),
				humanDuration(g.maxExpires.Sub(now)),
			)
			for _, r := range g.reservations {
				fmt.Fprintf(w, "    %s  %s\n", r.AddressGlob, r.Mode)
			}
		}
	}

	return 0
}

// renderStatusJSON emits the full structure as pretty-printed JSON
// for scripts. Field names are stable; the struct definitions in
// internal/store are the spec.
func renderStatusJSON(w io.Writer, st *store.StateStatus) int {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(st); err != nil {
		fmt.Fprintln(os.Stderr, "kl status: encode json:", err)
		return 1
	}
	return 0
}

// reservationGroup is one apply's slice of the reservations list,
// used so the table output groups by holder rather than scattering
// addresses across many rows-per-line.
type reservationGroup struct {
	applyID      string
	holder       string
	reservations []store.ActiveReservation
	maxExpires   time.Time
}

func groupReservationsByApply(rs []store.ActiveReservation) []reservationGroup {
	idx := make(map[string]*reservationGroup)
	for _, r := range rs {
		g, ok := idx[r.ApplyID]
		if !ok {
			g = &reservationGroup{applyID: r.ApplyID, holder: r.Holder}
			idx[r.ApplyID] = g
		}
		g.reservations = append(g.reservations, r)
		if r.ExpiresAt.After(g.maxExpires) {
			g.maxExpires = r.ExpiresAt
		}
	}
	out := make([]reservationGroup, 0, len(idx))
	for _, g := range idx {
		out = append(out, *g)
	}
	// Stable order: oldest acquisition first, then by apply uuid
	// for full determinism. Operators looking at a list of holders
	// generally want "who's been holding the longest" at the top.
	sort.Slice(out, func(i, j int) bool {
		if !out[i].maxExpires.Equal(out[j].maxExpires) {
			return out[i].maxExpires.Before(out[j].maxExpires)
		}
		return out[i].applyID < out[j].applyID
	})
	return out
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// humanDuration renders a positive duration without an "ago" suffix,
// for "X from now" phrasings like reservation lease expiry. Negative
// durations (already expired) render as "0s" so the operator-facing
// line stays readable even when the lease has just passed.
func humanDuration(d time.Duration) string {
	if d < 0 {
		return "0s"
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func countOrDash(p *int) string {
	if p == nil {
		return "-"
	}
	return fmt.Sprintf("%d", *p)
}

const statusUsage = `Usage:
  kl status [state] [flags]

Prints a one-screen summary of a state's live operational status:
identity, the v1 HTTP backend lock (if held), in-flight v2 apply
runs, and currently-held resource reservations. Designed to answer
"is my apply hanging, or just slow?" without writing SQL.

Positional:
  state                 State name (default: auto-detected from the
                        http backend address of the CWD).

Flags:
  --format=FMT          Output format: table (default) or json.
`
