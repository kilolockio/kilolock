//go:build integration

package store

import (
	"context"
	"testing"
	"time"

	"github.com/kilolockio/kilolock/pkg/testdb"
)

func TestGetStateStatus_OptimisticModeShowsAllLocksAndReservations(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	mustResetTables(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 15*time.Second)
	defer cancel()

	lineage := "9b39e2c0-aaaa-bbbb-cccc-000000000010"
	if err := s.WriteState(ctx, "qtest", "",
		seedOptimisticStateRaw(t, 1, lineage,
			[3]string{"aws_instance", "a", `{"v":1}`},
		),
		"test", "test",
	); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	if _, err := s.AcquireLock(ctx, "qtest", makeLockInfo("alice")); err != nil {
		t.Fatalf("alice AcquireLock: %v", err)
	}
	if _, err := s.AcquireLock(ctx, "qtest", makeLockInfo("bob")); err != nil {
		t.Fatalf("bob AcquireLock: %v", err)
	}

	info, err := s.GetCurrentStateInfo(ctx, "qtest")
	if err != nil {
		t.Fatalf("GetCurrentStateInfo: %v", err)
	}
	applyID := beginApply(t, s, info.StateID, info.VersionID, "ci")
	if err := s.AcquireReservations(ctx, info.StateID, applyID, "ci", []Reservation{
		{AddressGlob: "aws_instance.a", Mode: ReservationWrite},
	}, 5*time.Minute); err != nil {
		t.Fatalf("AcquireReservations: %v", err)
	}

	st, err := s.GetStateStatus(ctx, "qtest")
	if err != nil {
		t.Fatalf("GetStateStatus: %v", err)
	}
	if st.ExclusiveLocks {
		t.Fatalf("ExclusiveLocks = true, want false")
	}
	if st.CoexistenceMode != StateCoexistenceWarn {
		t.Fatalf("CoexistenceMode = %q, want %q", st.CoexistenceMode, StateCoexistenceWarn)
	}
	if len(st.Locks) != 2 {
		t.Fatalf("len(Locks) = %d, want 2", len(st.Locks))
	}
	if st.Lock == nil {
		t.Fatal("Lock = nil, want compatibility pointer")
	}
	if len(st.ActiveReservations) != 1 {
		t.Fatalf("len(ActiveReservations) = %d, want 1", len(st.ActiveReservations))
	}
	if len(st.InFlightApplies) != 1 {
		t.Fatalf("len(InFlightApplies) = %d, want 1", len(st.InFlightApplies))
	}
}

func TestStateCoexistenceMode_SetGet(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	mustResetTables(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 15*time.Second)
	defer cancel()

	lineage := "9b39e2c0-aaaa-bbbb-cccc-000000000011"
	if err := s.WriteState(ctx, "qtest", "",
		seedOptimisticStateRaw(t, 1, lineage,
			[3]string{"aws_instance", "a", `{"v":1}`},
		),
		"test", "test",
	); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	if err := s.SetStateCoexistenceMode(ctx, "qtest", StateCoexistenceStrict); err != nil {
		t.Fatalf("SetStateCoexistenceMode: %v", err)
	}
	got, err := s.GetStateCoexistenceMode(ctx, "qtest")
	if err != nil {
		t.Fatalf("GetStateCoexistenceMode: %v", err)
	}
	if got != StateCoexistenceStrict {
		t.Fatalf("GetStateCoexistenceMode = %q, want %q", got, StateCoexistenceStrict)
	}

	st, err := s.GetStateStatus(ctx, "qtest")
	if err != nil {
		t.Fatalf("GetStateStatus: %v", err)
	}
	if st.CoexistenceMode != StateCoexistenceStrict {
		t.Fatalf("StateStatus.CoexistenceMode = %q, want %q", st.CoexistenceMode, StateCoexistenceStrict)
	}
}
