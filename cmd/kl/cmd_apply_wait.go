package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/davesade/kilolock/internal/apply"
	"github.com/davesade/kilolock/pkg/store"
)

// newReservationWaitRenderer returns the apply.WaitEvent callback the
// CLI installs to stream wait-status lines to stderr. The renderer
// is deliberately conservative about its output:
//
//   - At most one block every minRenderInterval; intermediate events
//     are silently absorbed. The orchestrator's backoff cadence can
//     emit a callback every second early on, which would scroll the
//     operator's terminal too fast to read.
//
//   - The first event ALWAYS renders, regardless of interval — this
//     is the "we're waiting" announcement and skipping it would look
//     like the command hung.
//
//   - Re-renders ALSO fire whenever the conflict set CHANGES (a
//     blocker released or moved). Operators want immediate feedback
//     when their wait situation improves.
//
//   - Last 30s of the wait, we render every iteration regardless of
//     interval, so the "time to fail" countdown is visible.
//
// The renderer maintains a tiny bit of state (last-rendered time +
// last-rendered conflict signature) behind a mutex; it's safe to
// call from any goroutine although the orchestrator only calls it
// from one.
func newReservationWaitRenderer(w io.Writer, stateName string) func(apply.WaitEvent) {
	const minRenderInterval = 5 * time.Second
	const tailRenderWindow = 30 * time.Second

	var (
		mu            sync.Mutex
		lastRender    time.Time
		lastSignature string
	)

	return func(ev apply.WaitEvent) {
		mu.Lock()
		defer mu.Unlock()

		sig := conflictSignature(ev.Conflicts)
		now := time.Now()
		isFirst := lastRender.IsZero()
		conflictsChanged := sig != lastSignature
		isTail := ev.Remaining > 0 && ev.Remaining <= tailRenderWindow
		dueByInterval := now.Sub(lastRender) >= minRenderInterval

		if !isFirst && !conflictsChanged && !isTail && !dueByInterval {
			return
		}

		renderReservationWait(w, stateName, ev)
		lastRender = now
		lastSignature = sig
	}
}

// renderReservationWait writes a single wait-status block to stderr.
// Format:
//
//	[apply: waiting 12s/5m] blocked by 2 reservation(s) on "demo":
//	  aws_instance.web   write  held by alice (apply 93d94282, lease 2m38s)
//	  aws_instance.db    write  held by alice (apply 93d94282, lease 2m38s)
//	  retrying in 4s
//
// We render to stderr (not stdout) because the next thing the
// operator sees on stdout is terraform's own output once we have the
// reservations; mixing the wait notice into that stream is
// confusing.
func renderReservationWait(w io.Writer, stateName string, ev apply.WaitEvent) {
	fmt.Fprintf(w, "[apply: waiting %s/%s] blocked by %d reservation(s) on %q:\n",
		formatShort(ev.Elapsed),
		formatShort(ev.Elapsed+ev.Remaining),
		len(ev.Conflicts),
		stateName,
	)
	sorted := make([]store.ActiveReservation, len(ev.Conflicts))
	copy(sorted, ev.Conflicts)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].AddressGlob != sorted[j].AddressGlob {
			return sorted[i].AddressGlob < sorted[j].AddressGlob
		}
		return sorted[i].Mode < sorted[j].Mode
	})
	for _, c := range sorted {
		lease := time.Until(c.ExpiresAt)
		fmt.Fprintf(w, "  %s  %s  held by %s (apply %s, lease %s)\n",
			c.AddressGlob,
			c.Mode,
			emptyDash(c.Holder),
			shortUUID(c.ApplyID),
			formatShort(lease),
		)
	}
	if ev.Remaining > 0 {
		fmt.Fprintf(w, "  retrying in %s\n", formatShort(ev.NextRetryIn))
	} else {
		fmt.Fprintln(w, "  giving up (wait timeout exceeded)")
	}
}

// conflictSignature is the dedup key for the change-detect path.
// We don't care about the exact times — addresses + holders is the
// observable change an operator cares about ("alice released
// aws_instance.web, now only db is held"). The lease expiry shifts
// every second; including it would force a re-render every tick.
func conflictSignature(conflicts []store.ActiveReservation) string {
	if len(conflicts) == 0 {
		return ""
	}
	parts := make([]string, 0, len(conflicts))
	for _, c := range conflicts {
		parts = append(parts, fmt.Sprintf("%s/%s/%s/%s", c.AddressGlob, c.Mode, c.Holder, c.ApplyID))
	}
	sort.Strings(parts)
	return strings.Join(parts, "|")
}

// formatShort renders a duration in the most compact human form:
//
//	"42s" / "2m38s" / "12m" / "1h27m"
//
// Used in the wait-status block because lines get long fast with
// the standard Go "2m38.123456s" form.
func formatShort(d time.Duration) string {
	if d < 0 {
		return "0s"
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		secs := int(d.Seconds()) % 60
		mins := int(d.Minutes())
		if secs == 0 {
			return fmt.Sprintf("%dm", mins)
		}
		return fmt.Sprintf("%dm%ds", mins, secs)
	}
	hours := int(d.Hours())
	mins := int(d.Minutes()) % 60
	if mins == 0 {
		return fmt.Sprintf("%dh", hours)
	}
	return fmt.Sprintf("%dh%dm", hours, mins)
}
