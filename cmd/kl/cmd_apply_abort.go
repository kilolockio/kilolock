package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/davesade/kilolock/pkg/store"
)

func runApplyAbort(args []string) int {
	fs := flag.NewFlagSet("apply abort", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var (
		applyID = fs.String("apply-id", "", "Apply run id to abort (preferred).")
		state   = fs.String("state", "", "State name to search for a running apply to abort (requires --latest).")
		actor   = fs.String("actor", "", "Filter running applies by actor when using --state.")
		reason  = fs.String("reason", "aborted by operator", "Audit reason recorded on the apply run.")
		latest  = fs.Bool("latest", false, "When using --state, abort the most recent running apply.")
	)

	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "kl apply abort:", err)
		fmt.Fprint(os.Stderr, applyAbortUsage)
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "kl apply abort: unexpected extra arguments: %v\n", fs.Args())
		fmt.Fprint(os.Stderr, applyAbortUsage)
		return 2
	}

	ctx, cancel := context.WithTimeout(cliContext(), defaultTimeout)
	defer cancel()
	client, err := newAPIClient()
	if err != nil {
		fmt.Fprintln(os.Stderr, "kl apply abort:", err)
		return 1
	}

	var targetID string
	if strings.TrimSpace(*applyID) != "" {
		targetID = strings.TrimSpace(*applyID)
	} else {
		if strings.TrimSpace(*state) == "" || !*latest {
			fmt.Fprintln(os.Stderr, "kl apply abort: provide --apply-id, or use --state with --latest")
			fmt.Fprint(os.Stderr, applyAbortUsage)
			return 2
		}

		var status store.StateStatus
		if err := client.doJSON(ctx, "GET", "/admin/state/status?name="+queryEscape(strings.TrimSpace(*state)), strings.TrimSpace(*state), nil, &status); err != nil {
			fmt.Fprintln(os.Stderr, "kl apply abort:", err)
			return 1
		}
		if err != nil {
			return 1
		}
		runs := slices.Clone(status.InFlightApplies)
		wantActor := strings.TrimSpace(*actor)
		for _, r := range runs {
			if wantActor != "" && strings.TrimSpace(r.Actor) != wantActor {
				continue
			}
			targetID = r.ID
			break
		}
		if targetID == "" {
			fmt.Fprintf(os.Stderr, "kl apply abort: no running apply found for state=%q\n", strings.TrimSpace(*state))
			return 1
		}
	}

	if err := client.postJSON(ctx, "/admin/apply-runs/"+targetID+"/abort", strings.TrimSpace(*state), map[string]any{"reason": *reason}, nil); err != nil {
		if strings.Contains(err.Error(), "apply run not found") {
			fmt.Fprintf(os.Stderr, "kl apply abort: apply %s not found\n", targetID)
			return 1
		}
		if strings.Contains(err.Error(), "already finished") {
			fmt.Fprintf(os.Stderr, "kl apply abort: apply %s is already finished\n", targetID)
			return 1
		}
		fmt.Fprintln(os.Stderr, "kl apply abort:", err)
		return 1
	}
	_ = client.postJSON(ctx, "/admin/reservations/"+targetID+"/release", strings.TrimSpace(*state), map[string]any{}, nil)
	if strings.TrimSpace(*state) != "" && strings.TrimSpace(*applyID) == "" {
		fmt.Fprintf(os.Stdout, "aborted apply %s (state=%s)\n", targetID, strings.TrimSpace(*state))
	} else {
		fmt.Fprintf(os.Stdout, "aborted apply %s\n", targetID)
	}
	return 0
}

const applyAbortUsage = `Usage:
  kl apply abort --apply-id <uuid> [--reason "..."]
  kl apply abort --state <name> --latest [--actor <actor>] [--reason "..."]
`
