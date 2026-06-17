package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/kilolockio/kilolock/pkg/store"
)

// TestRenderStatusTable_NoLockNoApplies pins the all-clear shape.
// Most operator invocations of `kl status` should match
// this layout exactly — a stable visual signature for "nothing
// is wrong" matters because deviations from it are exactly what
// the operator scrolls back to confirm. The test asserts the
// presence of each header keyword, not exact whitespace, so
// future polish-passes don't trip it.
func TestRenderStatusTable_NoLockNoApplies(t *testing.T) {
	st := &store.StateStatus{
		Name:             "demo",
		Lineage:          "11111111-1111-1111-1111-111111111111",
		CurrentSerial:    7,
		TerraformVersion: "1.13.4",
		UpdatedAt:        time.Now().Add(-3 * time.Minute),
		ResourceCount:    42,
	}
	var buf bytes.Buffer
	if code := renderStatusTable(&buf, st); code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	got := buf.String()
	for _, want := range []string{
		"STATE  demo",
		"current serial    7",
		"coexistence mode  warn",
		"resources         42 alive",
		"LOCK  none",
		"IN-FLIGHT APPLIES  none",
		"RESERVATIONS  none held",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q\nFull output:\n%s", want, got)
		}
	}
}

// TestRenderStatusTable_LockHeldShowsRecoveryHint is the
// critical-feature test: when a lock is held, the output must
// include the exact psql one-liner an operator copies to clear
// a stale lock. The recovery hint is the single most-asked
// support question of "my terraform is hanging"; if a future
// edit removes it, this test fails the build.
func TestRenderStatusTable_LockHeldShowsRecoveryHint(t *testing.T) {
	now := time.Now()
	st := &store.StateStatus{
		Name:          "demo",
		CurrentSerial: 1,
		UpdatedAt:     now,
		Lock: &store.StatusLock{
			LockID:  "tf-lock-abc",
			Who:     "alice@laptop",
			Info:    "OperationTypeApply",
			Created: now.Add(-2 * time.Minute),
		},
	}
	var buf bytes.Buffer
	renderStatusTable(&buf, st)
	got := buf.String()

	for _, want := range []string{
		"LOCK  HELD",
		"tf-lock-abc",
		"alice@laptop",
		"DELETE FROM state_locks",
		// State name interpolated into the recovery hint, so the
		// operator can paste-and-run rather than substitute.
		"name = 'demo'",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("locked output missing %q\nFull output:\n%s", want, got)
		}
	}
}

func TestRenderStatusTable_MultipleLocksAndMixedModeWarning(t *testing.T) {
	now := time.Now()
	st := &store.StateStatus{
		Name:            "demo",
		CurrentSerial:   1,
		UpdatedAt:       now,
		CoexistenceMode: store.StateCoexistenceStrict,
		Locks: []store.StatusLock{
			{LockID: "tf-lock-a", Who: "alice@laptop", Info: "OperationTypeApply", Created: now.Add(-2 * time.Minute)},
			{LockID: "tf-lock-b", Who: "bob@laptop", Info: "OperationTypeApply", Created: now.Add(-1 * time.Minute)},
		},
		InFlightApplies: []store.StatusApplyRun{
			{ID: "apply-run-1", Actor: "ci", SourceSerial: 7, StartedAt: now.Add(-30 * time.Second)},
		},
	}
	var buf bytes.Buffer
	renderStatusTable(&buf, st)
	got := buf.String()
	for _, want := range []string{
		"LOCKS  2 held",
		"coexistence mode  strict",
		"alice@laptop",
		"bob@laptop",
		"COEXISTENCE WARNING",
		"mixed vanilla terraform + kl apply activity detected",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q\nFull output:\n%s", want, got)
		}
	}
}

// TestRenderStatusTable_ReservationsGroupedByApply: the
// grouping is what makes a multi-apply situation readable. Three
// reservations belonging to one apply must render under one
// "apply <id>" header; reservations from two applies render as
// two groups. Asserts both shape and order (oldest-expiring
// group first).
func TestRenderStatusTable_ReservationsGroupedByApply(t *testing.T) {
	now := time.Now()
	st := &store.StateStatus{
		Name:          "demo",
		CurrentSerial: 1,
		UpdatedAt:     now,
		ActiveReservations: []store.ActiveReservation{
			{ApplyID: "alice-apply-id", AddressGlob: "aws_vpc.main",
				Mode: store.ReservationRead, Holder: "alice",
				AcquiredAt: now.Add(-2 * time.Minute),
				ExpiresAt:  now.Add(60 * time.Second)},
			{ApplyID: "alice-apply-id", AddressGlob: "aws_subnet.public",
				Mode: store.ReservationWrite, Holder: "alice",
				AcquiredAt: now.Add(-2 * time.Minute),
				ExpiresAt:  now.Add(60 * time.Second)},
			{ApplyID: "bob-apply-id", AddressGlob: "aws_instance.db",
				Mode: store.ReservationWrite, Holder: "bob",
				AcquiredAt: now.Add(-1 * time.Minute),
				ExpiresAt:  now.Add(120 * time.Second)},
		},
	}
	var buf bytes.Buffer
	renderStatusTable(&buf, st)
	got := buf.String()

	// Both apply ids appear, both holders appear, all three
	// addresses appear.
	for _, want := range []string{
		"holder=alice", "holder=bob",
		"aws_vpc.main", "aws_subnet.public", "aws_instance.db",
		"RESERVATIONS  3 held",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("reservations output missing %q\nFull output:\n%s", want, got)
		}
	}

	// Alice's group's lease expires first (60s vs 120s), so her
	// "apply ...  (holder=alice" line must appear before bob's.
	aliceIdx := strings.Index(got, "holder=alice")
	bobIdx := strings.Index(got, "holder=bob")
	if aliceIdx < 0 || bobIdx < 0 {
		t.Fatal("could not locate holder lines in output")
	}
	if aliceIdx > bobIdx {
		t.Errorf("alice's apply (earlier-expiring lease) should render first; got alice@%d bob@%d", aliceIdx, bobIdx)
	}
}

// TestHumanDuration_PositiveDurations: pin the units. Operators
// glance at "lease expires in 5m" and instantly classify it as
// "still time to wait". Changing the unit format is a breaking
// change to that glance pattern.
func TestHumanDuration_PositiveDurations(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "30s"},
		{5 * time.Minute, "5m"},
		{2 * time.Hour, "2h"},
		{3 * 24 * time.Hour, "3d"},
	}
	for _, c := range cases {
		got := humanDuration(c.d)
		if got != c.want {
			t.Errorf("humanDuration(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

// TestHumanDuration_NegativeFloorsToZero: the lease-already-
// expired case. We choose 0s rather than e.g. "-3m" because the
// negative form distracts from the operator's actual decision
// (the reservation row is about to be reaped; the lease number is
// irrelevant).
func TestHumanDuration_NegativeFloorsToZero(t *testing.T) {
	if got := humanDuration(-5 * time.Minute); got != "0s" {
		t.Errorf("humanDuration(-5m) = %q, want 0s", got)
	}
}
