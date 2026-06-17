package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// TestReservationMode_Valid exists because the Postgres CHECK
// constraint is the last line of defense; this is the first.
// A caller that builds a Reservation with an unknown mode should
// fail fast in pure-Go before the DB round trip.
func TestReservationMode_Valid(t *testing.T) {
	for _, tc := range []struct {
		mode ReservationMode
		want bool
	}{
		{ReservationRead, true},
		{ReservationWrite, true},
		{"", false},
		{"READ", false},
		{"writes", false},
		{"admin", false},
	} {
		if got := tc.mode.Valid(); got != tc.want {
			t.Errorf("Valid(%q) = %v, want %v", tc.mode, got, tc.want)
		}
	}
}

// TestReservationConflictError_ErrorMessage asserts the operator-facing
// formatting contract: one line, semicolon-joined, with address/mode,
// holder, apply id and expires_at. Operators see this string straight
// through to their terminal — drift in formatting is a UX regression
// worth catching at test time.
func TestReservationConflictError_ErrorMessage(t *testing.T) {
	exp := time.Date(2026, 5, 14, 12, 30, 0, 0, time.UTC)
	err := &ReservationConflictError{
		StateID: "s1",
		Conflicts: []ActiveReservation{
			{
				AddressGlob: "random_id.web",
				Mode:        ReservationWrite,
				Holder:      "alice",
				ApplyID:     "ar-aaaa",
				ExpiresAt:   exp,
			},
			{
				AddressGlob: "random_id.db",
				Mode:        ReservationRead,
				Holder:      "bob",
				ApplyID:     "ar-bbbb",
				ExpiresAt:   exp,
			},
		},
	}
	got := err.Error()
	for _, want := range []string{
		"reservation conflict:",
		"random_id.web/write",
		"held by alice",
		"apply ar-aaaa",
		"random_id.db/read",
		"held by bob",
		"apply ar-bbbb",
		"2026-05-14T12:30:00Z",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Error() missing %q\nfull: %s", want, got)
		}
	}
}

// TestReservationConflictError_IsSentinel locks in the
// errors.Is(err, ErrReservationConflict) contract so callers can
// pattern-match on the sentinel without importing the typed struct.
// If this test fails, an upstream refactor probably broke the Is()
// method and callers in other packages will silently stop catching
// conflicts.
func TestReservationConflictError_IsSentinel(t *testing.T) {
	err := error(&ReservationConflictError{
		Conflicts: []ActiveReservation{{
			AddressGlob: "x",
			Mode:        ReservationWrite,
			Holder:      "a",
			ApplyID:     "b",
		}},
	})
	if !errors.Is(err, ErrReservationConflict) {
		t.Fatal("errors.Is(err, ErrReservationConflict) returned false; sentinel match broken")
	}
	if errors.Is(err, errors.New("unrelated")) {
		t.Fatal("Is matched an unrelated error; over-broad match")
	}
}

// TestReservationConflictError_EmptyConflictsFallsBack covers the
// defensive path: an empty Conflicts slice should still produce a
// recognizable message rather than the zero string. The acquire
// path never constructs an empty conflict error, but defensive code
// matters more in error paths than in happy paths.
func TestReservationConflictError_EmptyConflictsFallsBack(t *testing.T) {
	err := &ReservationConflictError{}
	if err.Error() == "" {
		t.Fatal("empty Error() string from zero-conflict error")
	}
	if !errors.Is(err, ErrReservationConflict) {
		t.Fatal("zero-conflict error does not satisfy sentinel match")
	}
}

// TestApplyRunStatus_IsTerminal pins the three terminal values to the
// IsTerminal predicate. The FinishApplyRun precondition relies on
// this; if 'committed' silently slipped out of the terminal set, the
// only failure would be the FK-violation error from the DB after a
// round trip — much worse than a fast Go-side assertion.
func TestApplyRunStatus_IsTerminal(t *testing.T) {
	cases := map[ApplyRunStatus]bool{
		ApplyRunRunning:   false,
		ApplyRunCommitted: true,
		ApplyRunFailed:    true,
		ApplyRunAborted:   true,
		"":                false,
		"succeeded":       false, // refresh_runs uses this; apply_runs does not
	}
	for s, want := range cases {
		if got := s.IsTerminal(); got != want {
			t.Errorf("IsTerminal(%q) = %v, want %v", s, got, want)
		}
	}
}
