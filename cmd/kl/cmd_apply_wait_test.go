package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/davesade/kilolock/internal/apply"
	"github.com/davesade/kilolock/pkg/store"
)

func mkConflict(addr, mode, holder, applyID string, leaseSecs int) store.ActiveReservation {
	return store.ActiveReservation{
		ApplyID:     applyID,
		AddressGlob: addr,
		Mode:        store.ReservationMode(mode),
		Holder:      holder,
		AcquiredAt:  time.Now().Add(-1 * time.Minute),
		ExpiresAt:   time.Now().Add(time.Duration(leaseSecs) * time.Second),
	}
}

// TestRenderReservationWait_Format pins the operator-facing layout
// of the wait block. We're not enforcing exact whitespace (tabwriter
// is involved indirectly) but the key labels and the affected
// resources MUST appear.
func TestRenderReservationWait_Format(t *testing.T) {
	var buf bytes.Buffer
	ev := apply.WaitEvent{
		Iteration: 1, Elapsed: 12 * time.Second, Remaining: 4*time.Minute + 48*time.Second,
		NextRetryIn: 2 * time.Second,
		Conflicts: []store.ActiveReservation{
			mkConflict("aws_instance.web", "write", "alice", "93d94282-1111-1111-1111-111111111111", 158),
			mkConflict("aws_instance.db", "write", "alice", "93d94282-1111-1111-1111-111111111111", 158),
		},
	}
	renderReservationWait(&buf, "demo", ev)
	out := buf.String()

	for _, want := range []string{
		"waiting 12s/5m",
		"blocked by 2 reservation(s) on \"demo\"",
		"aws_instance.db", "aws_instance.web",
		"held by alice",
		"apply 93d94282",
		"retrying in 2s",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q\nfull output:\n%s", want, out)
		}
	}
	// Addresses must render in sorted order: db before web.
	dbIdx := strings.Index(out, "aws_instance.db")
	webIdx := strings.Index(out, "aws_instance.web")
	if dbIdx == -1 || webIdx == -1 || dbIdx > webIdx {
		t.Errorf("addresses not sorted: db@%d web@%d", dbIdx, webIdx)
	}
}

// TestRenderReservationWait_FinalIterationSaysGivingUp: when
// Remaining == 0, the block must change from "retrying in X" to
// "giving up". This is the contract that ensures the operator can
// see immediately when the wait is about to surface an error.
func TestRenderReservationWait_FinalIterationSaysGivingUp(t *testing.T) {
	var buf bytes.Buffer
	ev := apply.WaitEvent{
		Iteration: 5, Elapsed: 5 * time.Minute, Remaining: 0,
		NextRetryIn: 0,
		Conflicts: []store.ActiveReservation{
			mkConflict("aws_instance.web", "write", "bob", "abc", 30),
		},
	}
	renderReservationWait(&buf, "demo", ev)
	out := buf.String()
	if !strings.Contains(out, "giving up") {
		t.Errorf("final-iteration output missing 'giving up'; got:\n%s", out)
	}
	if strings.Contains(out, "retrying in") {
		t.Errorf("final-iteration output should NOT mention 'retrying in':\n%s", out)
	}
}

// TestNewReservationWaitRenderer_ThrottlesOutput exercises the
// dedup/throttle logic. Three callbacks fired in quick succession
// with the same conflict set should produce exactly ONE rendered
// block (the first one), not three — operators don't want their
// terminal scrolling past one-second updates.
func TestNewReservationWaitRenderer_ThrottlesOutput(t *testing.T) {
	var buf bytes.Buffer
	r := newReservationWaitRenderer(&buf, "demo")
	conflicts := []store.ActiveReservation{
		mkConflict("aws_instance.web", "write", "alice", "abc", 100),
	}
	r(apply.WaitEvent{Iteration: 1, Elapsed: 0, Remaining: time.Minute, NextRetryIn: time.Second, Conflicts: conflicts})
	r(apply.WaitEvent{Iteration: 2, Elapsed: time.Second, Remaining: 59 * time.Second, NextRetryIn: 2 * time.Second, Conflicts: conflicts})
	r(apply.WaitEvent{Iteration: 3, Elapsed: 2 * time.Second, Remaining: 58 * time.Second, NextRetryIn: 4 * time.Second, Conflicts: conflicts})

	// First should render, next two should not (same signature, no
	// interval/tail trigger). So we should see exactly ONE
	// "blocked by" header.
	headerCount := strings.Count(buf.String(), "blocked by ")
	if headerCount != 1 {
		t.Errorf("got %d blocks, want 1 (throttle is broken):\n%s", headerCount, buf.String())
	}
}

// TestNewReservationWaitRenderer_RendersOnConflictChange: the
// signature-change branch — if a blocker releases, the operator
// MUST see that immediately, not after the throttle interval.
func TestNewReservationWaitRenderer_RendersOnConflictChange(t *testing.T) {
	var buf bytes.Buffer
	r := newReservationWaitRenderer(&buf, "demo")
	first := []store.ActiveReservation{
		mkConflict("aws_instance.web", "write", "alice", "abc", 100),
		mkConflict("aws_instance.db", "write", "alice", "abc", 100),
	}
	second := []store.ActiveReservation{
		// db released; only web remains
		mkConflict("aws_instance.web", "write", "alice", "abc", 100),
	}
	r(apply.WaitEvent{Iteration: 1, Elapsed: 0, Remaining: time.Minute, NextRetryIn: time.Second, Conflicts: first})
	r(apply.WaitEvent{Iteration: 2, Elapsed: time.Second, Remaining: 59 * time.Second, NextRetryIn: 2 * time.Second, Conflicts: second})

	headerCount := strings.Count(buf.String(), "blocked by ")
	if headerCount != 2 {
		t.Errorf("got %d blocks, want 2 (change-detect is broken):\n%s", headerCount, buf.String())
	}
	if !strings.Contains(buf.String(), "blocked by 2 reservation") || !strings.Contains(buf.String(), "blocked by 1 reservation") {
		t.Errorf("expected both 2-blocker and 1-blocker renders:\n%s", buf.String())
	}
}

// TestFormatShort: the duration formatter. Operators see this
// dozens of times per apply on a busy state; ugly formatting
// (e.g. "2m38.123456s" instead of "2m38s") hurts more than the
// effort to test pins it.
func TestFormatShort(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{-5 * time.Second, "0s"},
		{45 * time.Second, "45s"},
		{1 * time.Minute, "1m"},
		{2*time.Minute + 38*time.Second, "2m38s"},
		{1 * time.Hour, "1h"},
		{1*time.Hour + 27*time.Minute, "1h27m"},
	}
	for _, c := range cases {
		if got := formatShort(c.d); got != c.want {
			t.Errorf("formatShort(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}
