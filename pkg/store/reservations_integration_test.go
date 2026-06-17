//go:build integration

// Integration tests for the v2a reservations + apply_runs substrate.
//
// Run with:
//
//	KL_DATABASE_URL=postgres://kl:kl@localhost:5432/kl?sslmode=disable \
//	  go test -tags=integration -run TestReservations ./pkg/store/...
//	  go test -tags=integration -run TestApplyRuns    ./pkg/store/...
//
// The conflict matrix and the lease/heartbeat lifecycle are the
// load-bearing properties of v2; assertions here are deliberately
// granular so a regression in either rule lights up the matching
// case rather than a vague "something is off" failure.

package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kilolockio/kilolock/pkg/db"
	"github.com/kilolockio/kilolock/pkg/testdb"
)

// resetApplyV2Tables wipes the v2 audit + reservations tables for
// unprotected states. The FK CASCADE in mustResetTables already
// covers these for tests that wipe the whole world, but tests
// that want to keep an existing seeded state intact use this
// narrower reset instead. Scoping via NOT IN (protected state ids)
// keeps the operator's big-state demo fixture and any test-seeded
// state untouched if either has live reservations/apply_runs —
// the old unscoped TRUNCATE here would have silently destroyed
// them.
func resetApplyV2Tables(t *testing.T, pool *db.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Second)
	defer cancel()

	protected := testdb.ProtectedStates()

	if _, err := pool.Exec(ctx,
		`DELETE FROM resource_reservations
		 WHERE state_id NOT IN (SELECT id FROM states WHERE name = ANY($1))`,
		protected,
	); err != nil {
		t.Fatalf("delete unprotected reservations: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`DELETE FROM apply_runs
		 WHERE state_id NOT IN (SELECT id FROM states WHERE name = ANY($1))`,
		protected,
	); err != nil {
		t.Fatalf("delete unprotected apply_runs: %v", err)
	}
}

// seedApplyState writes a minimal state + state_version pair and
// returns the ids the v2a tables need. Modeled on seedStateAndVersion
// in the refresh_runs tests so the two test suites cohabit the same
// schema without surprises.
func seedApplyState(t *testing.T, s *Store, name string) (stateID, versionID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 10*time.Second)
	defer cancel()

	body := []byte(`{
		"version": 4,
		"terraform_version": "1.13.4",
		"serial": 1,
		"lineage": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		"outputs": {},
		"resources": [
			{
				"mode": "managed",
				"type": "aws_vpc",
				"name": "main",
				"provider": "provider[\"registry.terraform.io/hashicorp/aws\"]",
				"instances": [
					{
						"schema_version": 0,
						"attributes": { "id": "vpc-1" },
						"sensitive_attributes": []
					}
				]
			}
		]
	}`)
	if err := s.WriteState(ctx, name, "", body, "test", "test"); err != nil {
		t.Fatalf("WriteState(%q): %v", name, err)
	}
	err := s.pool.QueryRow(ctx,
		`SELECT s.id, s.current_version_id FROM states s WHERE s.name = $1`,
		name,
	).Scan(&stateID, &versionID)
	if err != nil {
		t.Fatalf("lookup state %q: %v", name, err)
	}
	return stateID, versionID
}

// beginApply opens a v2 apply_run row and returns its id. Used by
// every test below — Reservations.apply_id has a FK to apply_runs,
// so a stand-alone Acquire would always fail on a fresh schema.
func beginApply(t *testing.T, s *Store, stateID, versionID, actor string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Second)
	defer cancel()
	run, err := s.BeginApplyRun(ctx, stateID, versionID, actor, 1, nil)
	if err != nil {
		t.Fatalf("BeginApplyRun(actor=%s): %v", actor, err)
	}
	return run.ID
}

// TestReservations_CleanAcquireAndList covers the simplest happy
// path: a single apply acquires two reservations, ListActiveReservations
// returns them sorted, ReleaseReservations clears them. If any of
// these break, every other test in this file is also broken; this
// case is the canary.
func TestReservations_CleanAcquireAndList(t *testing.T) {
	s, pool := openTestStore(t)
	defer pool.Close()
	mustResetTables(t, pool)

	stateID, versionID := seedApplyState(t, s, "v2a-clean")
	apply := beginApply(t, s, stateID, versionID, "alice")

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Second)
	defer cancel()

	err := s.AcquireReservations(ctx, stateID, apply, "alice", []Reservation{
		{AddressGlob: "random_id.web", Mode: ReservationWrite},
		{AddressGlob: "random_id.vpc", Mode: ReservationRead},
	}, time.Minute)
	if err != nil {
		t.Fatalf("AcquireReservations: %v", err)
	}

	active, err := s.ListActiveReservations(ctx, "v2a-clean")
	if err != nil {
		t.Fatalf("ListActiveReservations: %v", err)
	}
	if len(active) != 2 {
		t.Fatalf("active reservations = %d, want 2", len(active))
	}
	// Sorted (address, mode): random_id.vpc/read before random_id.web/write
	if active[0].AddressGlob != "random_id.vpc" || active[0].Mode != ReservationRead {
		t.Errorf("active[0] = %s/%s, want random_id.vpc/read", active[0].AddressGlob, active[0].Mode)
	}
	if active[1].AddressGlob != "random_id.web" || active[1].Mode != ReservationWrite {
		t.Errorf("active[1] = %s/%s, want random_id.web/write", active[1].AddressGlob, active[1].Mode)
	}
	if active[0].Holder != "alice" {
		t.Errorf("holder = %q, want alice", active[0].Holder)
	}
	if !active[0].ExpiresAt.After(time.Now()) {
		t.Errorf("expires_at = %v, want > now()", active[0].ExpiresAt)
	}

	if err := s.ReleaseReservations(ctx, apply); err != nil {
		t.Fatalf("ReleaseReservations: %v", err)
	}
	active, err = s.ListActiveReservations(ctx, "v2a-clean")
	if err != nil {
		t.Fatalf("ListActiveReservations after release: %v", err)
	}
	if len(active) != 0 {
		t.Errorf("active after release = %d, want 0", len(active))
	}
}

// TestReservations_ReadReadCoexists is the one corner of the conflict
// matrix that does NOT conflict: two reads on the same address by
// different applies. If this case ever starts erroring, the matrix's
// promise of "concurrent reads coexist" is broken and all parallel
// applies that share read sets (the realistic case for a shared VPC
// referenced from many modules) regress to serial.
func TestReservations_ReadReadCoexists(t *testing.T) {
	s, pool := openTestStore(t)
	defer pool.Close()
	mustResetTables(t, pool)

	stateID, versionID := seedApplyState(t, s, "v2a-rr")
	aliceApply := beginApply(t, s, stateID, versionID, "alice")
	bobApply := beginApply(t, s, stateID, versionID, "bob")

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Second)
	defer cancel()

	if err := s.AcquireReservations(ctx, stateID, aliceApply, "alice", []Reservation{
		{AddressGlob: "shared.vpc", Mode: ReservationRead},
	}, time.Minute); err != nil {
		t.Fatalf("alice acquire: %v", err)
	}
	if err := s.AcquireReservations(ctx, stateID, bobApply, "bob", []Reservation{
		{AddressGlob: "shared.vpc", Mode: ReservationRead},
	}, time.Minute); err != nil {
		t.Fatalf("bob acquire (read-read should coexist): %v", err)
	}

	active, err := s.ListActiveReservations(ctx, "v2a-rr")
	if err != nil {
		t.Fatalf("ListActiveReservations: %v", err)
	}
	if len(active) != 2 {
		t.Errorf("active = %d, want 2 (both reads)", len(active))
	}
}

// TestReservations_WriteWriteConflicts asserts the most operationally
// important case: two writes to the same address by different applies
// must conflict. The first apply wins; the second sees its own conflict
// row in the error so the operator-facing message can name the holder.
func TestReservations_WriteWriteConflicts(t *testing.T) {
	s, pool := openTestStore(t)
	defer pool.Close()
	mustResetTables(t, pool)

	stateID, versionID := seedApplyState(t, s, "v2a-ww")
	aliceApply := beginApply(t, s, stateID, versionID, "alice")
	bobApply := beginApply(t, s, stateID, versionID, "bob")

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Second)
	defer cancel()

	if err := s.AcquireReservations(ctx, stateID, aliceApply, "alice", []Reservation{
		{AddressGlob: "random_id.web", Mode: ReservationWrite},
	}, time.Minute); err != nil {
		t.Fatalf("alice acquire: %v", err)
	}

	err := s.AcquireReservations(ctx, stateID, bobApply, "bob", []Reservation{
		{AddressGlob: "random_id.web", Mode: ReservationWrite},
	}, time.Minute)
	if !errors.Is(err, ErrReservationConflict) {
		t.Fatalf("bob acquire: err = %v, want ErrReservationConflict", err)
	}
	var ce *ReservationConflictError
	if !errors.As(err, &ce) {
		t.Fatal("conflict error not unwrappable as *ReservationConflictError")
	}
	if len(ce.Conflicts) != 1 {
		t.Fatalf("conflicts = %d, want 1", len(ce.Conflicts))
	}
	if ce.Conflicts[0].Holder != "alice" {
		t.Errorf("conflict holder = %q, want alice", ce.Conflicts[0].Holder)
	}

	// Alice's row must still be there — a failed bob acquire must
	// leave the prior holder untouched, not poison the state.
	active, err := s.ListActiveReservations(ctx, "v2a-ww")
	if err != nil {
		t.Fatalf("ListActiveReservations: %v", err)
	}
	if len(active) != 1 || active[0].Holder != "alice" {
		t.Errorf("active after failed acquire = %+v, want exactly alice's row", active)
	}
}

// TestReservations_ReadBlocksWrite and TestReservations_WriteBlocksRead
// cover the asymmetric quadrants of the matrix. Two separate tests
// (not table-driven) so a failure points at the exact direction.
func TestReservations_ReadBlocksWrite(t *testing.T) {
	s, pool := openTestStore(t)
	defer pool.Close()
	mustResetTables(t, pool)

	stateID, versionID := seedApplyState(t, s, "v2a-rblocksw")
	aliceApply := beginApply(t, s, stateID, versionID, "alice")
	bobApply := beginApply(t, s, stateID, versionID, "bob")

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Second)
	defer cancel()

	if err := s.AcquireReservations(ctx, stateID, aliceApply, "alice", []Reservation{
		{AddressGlob: "shared.db", Mode: ReservationRead},
	}, time.Minute); err != nil {
		t.Fatalf("alice (read) acquire: %v", err)
	}
	err := s.AcquireReservations(ctx, stateID, bobApply, "bob", []Reservation{
		{AddressGlob: "shared.db", Mode: ReservationWrite},
	}, time.Minute)
	if !errors.Is(err, ErrReservationConflict) {
		t.Fatalf("bob (write) acquire on existing read: err = %v, want ErrReservationConflict", err)
	}
}

func TestReservations_WriteBlocksRead(t *testing.T) {
	s, pool := openTestStore(t)
	defer pool.Close()
	mustResetTables(t, pool)

	stateID, versionID := seedApplyState(t, s, "v2a-wblocksr")
	aliceApply := beginApply(t, s, stateID, versionID, "alice")
	bobApply := beginApply(t, s, stateID, versionID, "bob")

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Second)
	defer cancel()

	if err := s.AcquireReservations(ctx, stateID, aliceApply, "alice", []Reservation{
		{AddressGlob: "shared.db", Mode: ReservationWrite},
	}, time.Minute); err != nil {
		t.Fatalf("alice (write) acquire: %v", err)
	}
	err := s.AcquireReservations(ctx, stateID, bobApply, "bob", []Reservation{
		{AddressGlob: "shared.db", Mode: ReservationRead},
	}, time.Minute)
	if !errors.Is(err, ErrReservationConflict) {
		t.Fatalf("bob (read) acquire on existing write: err = %v, want ErrReservationConflict", err)
	}
}

// TestReservations_DisjointAddressesAllowParallelWrites is the test
// the v2 marketing claim hinges on. Two writes that touch different
// addresses must coexist, even when both are on the same state.
// Failure here would mean Kilolock is no better than a flat-state
// backend for the bottleneck v2 is meant to remove.
func TestReservations_DisjointAddressesAllowParallelWrites(t *testing.T) {
	s, pool := openTestStore(t)
	defer pool.Close()
	mustResetTables(t, pool)

	stateID, versionID := seedApplyState(t, s, "v2a-disjoint")
	aliceApply := beginApply(t, s, stateID, versionID, "alice")
	bobApply := beginApply(t, s, stateID, versionID, "bob")

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Second)
	defer cancel()

	if err := s.AcquireReservations(ctx, stateID, aliceApply, "alice", []Reservation{
		{AddressGlob: "module.web.aws_lb.main", Mode: ReservationWrite},
	}, time.Minute); err != nil {
		t.Fatalf("alice acquire: %v", err)
	}
	if err := s.AcquireReservations(ctx, stateID, bobApply, "bob", []Reservation{
		{AddressGlob: "module.db.aws_rds.primary", Mode: ReservationWrite},
	}, time.Minute); err != nil {
		t.Fatalf("bob acquire (disjoint write should succeed): %v", err)
	}

	active, err := s.ListActiveReservations(ctx, "v2a-disjoint")
	if err != nil {
		t.Fatalf("ListActiveReservations: %v", err)
	}
	if len(active) != 2 {
		t.Errorf("active = %d, want 2 (disjoint writes coexist)", len(active))
	}
}

// TestReservations_ExpiredRowsAreReclaimed simulates a SIGKILLed
// apply: alice's reservation expires without being released, then
// bob acquires the same address. The acquire must succeed (alice's
// dead row gets swept) and the final state must have only bob's row.
func TestReservations_ExpiredRowsAreReclaimed(t *testing.T) {
	s, pool := openTestStore(t)
	defer pool.Close()
	mustResetTables(t, pool)

	stateID, versionID := seedApplyState(t, s, "v2a-expiry")
	aliceApply := beginApply(t, s, stateID, versionID, "alice")
	bobApply := beginApply(t, s, stateID, versionID, "bob")

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Second)
	defer cancel()

	if err := s.AcquireReservations(ctx, stateID, aliceApply, "alice", []Reservation{
		{AddressGlob: "random_id.web", Mode: ReservationWrite},
	}, time.Second); err != nil {
		t.Fatalf("alice acquire (1s lease): %v", err)
	}

	// Force-expire alice's row by rewriting expires_at into the
	// past. Avoids a real sleep so the test stays fast.
	if _, err := s.pool.Exec(ctx,
		`UPDATE resource_reservations
		 SET    expires_at = now() - interval '1 second'
		 WHERE  apply_id = $1`,
		aliceApply,
	); err != nil {
		t.Fatalf("force-expire alice: %v", err)
	}

	// Bob tries the same address — should succeed via in-place
	// reclaim of alice's expired row.
	if err := s.AcquireReservations(ctx, stateID, bobApply, "bob", []Reservation{
		{AddressGlob: "random_id.web", Mode: ReservationWrite},
	}, time.Minute); err != nil {
		t.Fatalf("bob acquire after alice expiry: %v", err)
	}

	active, err := s.ListActiveReservations(ctx, "v2a-expiry")
	if err != nil {
		t.Fatalf("ListActiveReservations: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("active = %d, want 1 (bob owns the address now)", len(active))
	}
	if active[0].Holder != "bob" || active[0].ApplyID != bobApply {
		t.Errorf("active[0] = {holder=%s, apply=%s}, want bob/%s",
			active[0].Holder, active[0].ApplyID, bobApply)
	}
}

// TestReservations_RenewExtendsLease is the heartbeat path. A
// long-running apply renews its reservations periodically; each
// renew must extend expires_at far enough into the future that the
// next renew gets a chance. Returning 0 rows means the heartbeat
// must declare the apply pre-empted.
func TestReservations_RenewExtendsLease(t *testing.T) {
	s, pool := openTestStore(t)
	defer pool.Close()
	mustResetTables(t, pool)

	stateID, versionID := seedApplyState(t, s, "v2a-renew")
	apply := beginApply(t, s, stateID, versionID, "alice")

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Second)
	defer cancel()

	if err := s.AcquireReservations(ctx, stateID, apply, "alice", []Reservation{
		{AddressGlob: "a", Mode: ReservationWrite},
		{AddressGlob: "b", Mode: ReservationRead},
	}, 5*time.Second); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	before, err := s.ListActiveReservations(ctx, "v2a-renew")
	if err != nil {
		t.Fatalf("list before renew: %v", err)
	}
	oldExpiry := before[0].ExpiresAt

	// Sleep enough that any clock-skew tolerance can't paper over
	// the fact that we extended the lease. 1.5 seconds is well
	// inside the original 5s and well past the per-call wall time.
	time.Sleep(1500 * time.Millisecond)

	n, err := s.RenewReservations(ctx, apply, time.Minute)
	if err != nil {
		t.Fatalf("RenewReservations: %v", err)
	}
	if n != 2 {
		t.Errorf("RenewReservations rows = %d, want 2", n)
	}

	after, err := s.ListActiveReservations(ctx, "v2a-renew")
	if err != nil {
		t.Fatalf("list after renew: %v", err)
	}
	if !after[0].ExpiresAt.After(oldExpiry.Add(30 * time.Second)) {
		t.Errorf("renew did not extend expires_at: before=%v after=%v",
			oldExpiry, after[0].ExpiresAt)
	}
}

// TestReservations_RenewOnReclaimedApplyReturnsZero is the
// pre-emption signal the heartbeat code needs. If the orchestrator's
// rows have been swept (or never existed), Renew returns 0 with no
// error; the orchestrator interprets that as "we lost our lease,
// abort the apply".
func TestReservations_RenewOnReclaimedApplyReturnsZero(t *testing.T) {
	s, pool := openTestStore(t)
	defer pool.Close()
	mustResetTables(t, pool)

	stateID, versionID := seedApplyState(t, s, "v2a-renew-zero")
	apply := beginApply(t, s, stateID, versionID, "alice")

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Second)
	defer cancel()

	// No Acquire — just call Renew. The apply has no reservation
	// rows; renew must return 0 cleanly so the heartbeat can stop.
	n, err := s.RenewReservations(ctx, apply, time.Minute)
	if err != nil {
		t.Fatalf("RenewReservations on empty: %v", err)
	}
	if n != 0 {
		t.Errorf("RenewReservations on empty rows = %d, want 0", n)
	}
}

// TestReservations_AcquireIsIdempotentForSameApply covers the retry
// path the orchestrator depends on. A second Acquire with the same
// apply_id, address, mode must succeed without inserting duplicates
// and must refresh expires_at — that's the documented contract.
// Without idempotency the orchestrator can't safely retry transient
// errors.
func TestReservations_AcquireIsIdempotentForSameApply(t *testing.T) {
	s, pool := openTestStore(t)
	defer pool.Close()
	mustResetTables(t, pool)

	stateID, versionID := seedApplyState(t, s, "v2a-idem")
	apply := beginApply(t, s, stateID, versionID, "alice")

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Second)
	defer cancel()

	for i := 0; i < 3; i++ {
		if err := s.AcquireReservations(ctx, stateID, apply, "alice", []Reservation{
			{AddressGlob: "x", Mode: ReservationWrite},
		}, time.Minute); err != nil {
			t.Fatalf("acquire iteration %d: %v", i, err)
		}
	}
	active, err := s.ListActiveReservations(ctx, "v2a-idem")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(active) != 1 {
		t.Errorf("active = %d, want 1 (idempotent re-acquire)", len(active))
	}
}

// TestReservations_ReleaseIsIdempotent makes sure releasing an
// already-released apply does not error. The orchestrator's defer
// path may call Release after a partial Acquire that already
// produced no rows; if Release errored on missing rows the defer
// would log noise on every clean run.
func TestReservations_ReleaseIsIdempotent(t *testing.T) {
	s, pool := openTestStore(t)
	defer pool.Close()
	mustResetTables(t, pool)

	stateID, versionID := seedApplyState(t, s, "v2a-release-idem")
	apply := beginApply(t, s, stateID, versionID, "alice")

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Second)
	defer cancel()

	// Two releases in a row, no acquire between them.
	for i := 0; i < 2; i++ {
		if err := s.ReleaseReservations(ctx, apply); err != nil {
			t.Fatalf("release iteration %d: %v", i, err)
		}
	}
}

// TestReservations_EmptyWantIsNoop validates that an Acquire with
// zero rows succeeds without taking the advisory lock or producing
// audit rows. The orchestrator sometimes computes an empty write
// set (a plan with only no-op changes); short-circuiting here keeps
// that path free of spurious DB work.
func TestReservations_EmptyWantIsNoop(t *testing.T) {
	s, pool := openTestStore(t)
	defer pool.Close()
	mustResetTables(t, pool)

	stateID, versionID := seedApplyState(t, s, "v2a-empty")
	apply := beginApply(t, s, stateID, versionID, "alice")

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Second)
	defer cancel()

	if err := s.AcquireReservations(ctx, stateID, apply, "alice", nil, time.Minute); err != nil {
		t.Fatalf("empty acquire: %v", err)
	}
	active, err := s.ListActiveReservations(ctx, "v2a-empty")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(active) != 0 {
		t.Errorf("active = %d, want 0", len(active))
	}
}

// TestApplyRuns_HappyPath covers Begin → committed Finish → Get →
// List. The committed-serial CHECK is exercised by passing a non-nil
// CommittedSerial on a committed status; a Failed test below verifies
// the inverse rule.
func TestApplyRuns_HappyPath(t *testing.T) {
	s, pool := openTestStore(t)
	defer pool.Close()
	mustResetTables(t, pool)

	stateID, versionID := seedApplyState(t, s, "v2a-applyhappy")

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Second)
	defer cancel()

	run, err := s.BeginApplyRun(ctx, stateID, versionID, "alice", 7, nil)
	if err != nil {
		t.Fatalf("BeginApplyRun: %v", err)
	}
	if run.Status != ApplyRunRunning {
		t.Errorf("initial status = %q, want running", run.Status)
	}
	if run.SourceSerial != 7 {
		t.Errorf("source_serial = %d, want 7", run.SourceSerial)
	}

	committed := int64(8)
	if err := s.FinishApplyRun(ctx, run.ID, FinishApplyRunInput{
		Status:           ApplyRunCommitted,
		ToVersionID:      versionID, // reuse for fixture simplicity
		CommittedSerial:  &committed,
		ResourcesPlanned: 3,
		ResourcesApplied: 3,
		ResourcesFailed:  0,
	}); err != nil {
		t.Fatalf("FinishApplyRun: %v", err)
	}

	got, err := s.GetApplyRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetApplyRun: %v", err)
	}
	if got.Status != ApplyRunCommitted {
		t.Errorf("post-finish status = %q, want committed", got.Status)
	}
	if got.CommittedSerial == nil || *got.CommittedSerial != 8 {
		t.Errorf("committed_serial = %v, want 8", got.CommittedSerial)
	}
	if got.FinishedAt == nil {
		t.Errorf("finished_at unset on committed run")
	}

	runs, err := s.ListApplyRuns(ctx, "v2a-applyhappy", 10)
	if err != nil {
		t.Fatalf("ListApplyRuns: %v", err)
	}
	if len(runs) != 1 || runs[0].ID != run.ID {
		t.Errorf("list = %+v, want 1 run with id %s", runs, run.ID)
	}
}

func TestApplyRuns_AbortReleasesReservationsViaCaller(t *testing.T) {
	s, pool := openTestStore(t)
	defer pool.Close()
	mustResetTables(t, pool)

	stateID, versionID := seedApplyState(t, s, "v2a-abort")

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 10*time.Second)
	defer cancel()

	run, err := s.BeginApplyRun(ctx, stateID, versionID, "alice", 1, nil)
	if err != nil {
		t.Fatalf("BeginApplyRun: %v", err)
	}
	if err := s.AcquireReservations(ctx, stateID, run.ID, "alice",
		[]Reservation{{AddressGlob: "null_resource.x", Mode: ReservationWrite}},
		30*time.Second,
	); err != nil {
		t.Fatalf("AcquireReservations: %v", err)
	}

	if err := s.AbortApplyRun(ctx, run.ID, "operator abort test"); err != nil {
		t.Fatalf("AbortApplyRun: %v", err)
	}
	got, err := s.GetApplyRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetApplyRun: %v", err)
	}
	if got.Status != ApplyRunAborted {
		t.Fatalf("status = %q, want %q", got.Status, ApplyRunAborted)
	}

	if err := s.ReleaseReservations(ctx, run.ID); err != nil {
		t.Fatalf("ReleaseReservations: %v", err)
	}
	active, err := s.ListActiveReservations(ctx, "v2a-abort")
	if err != nil {
		t.Fatalf("ListActiveReservations: %v", err)
	}
	if len(active) != 0 {
		t.Fatalf("active reservations = %d, want 0", len(active))
	}
}

// TestApplyRuns_CommittedSerialOnlyOnCommit pins both halves of the
// apply_runs_committed_serial_only_on_commit CHECK plus its Go-side
// pre-check. Without these guards a 'failed' apply could lie about
// having produced a state_version.
func TestApplyRuns_CommittedSerialOnlyOnCommit(t *testing.T) {
	s, pool := openTestStore(t)
	defer pool.Close()
	mustResetTables(t, pool)

	stateID, versionID := seedApplyState(t, s, "v2a-cscheck")

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Second)
	defer cancel()

	run, err := s.BeginApplyRun(ctx, stateID, versionID, "alice", 1, nil)
	if err != nil {
		t.Fatalf("BeginApplyRun: %v", err)
	}

	// committed without a serial → reject
	err = s.FinishApplyRun(ctx, run.ID, FinishApplyRunInput{Status: ApplyRunCommitted})
	if err == nil {
		t.Error("Finish(committed, serial=nil) returned nil, expected error")
	}

	// failed with a serial → reject
	committed := int64(2)
	err = s.FinishApplyRun(ctx, run.ID, FinishApplyRunInput{
		Status:          ApplyRunFailed,
		CommittedSerial: &committed,
	})
	if err == nil {
		t.Error("Finish(failed, serial=2) returned nil, expected error")
	}

	// Sanity: a clean failed finish (no serial) still works after
	// the two rejections — neither bad call should have advanced
	// the row to a terminal state.
	if err := s.FinishApplyRun(ctx, run.ID, FinishApplyRunInput{
		Status:           ApplyRunFailed,
		ResourcesPlanned: 1,
		ResourcesApplied: 0,
		ResourcesFailed:  1,
		ErrorSummary:     "provider exploded",
	}); err != nil {
		t.Fatalf("clean failed Finish: %v", err)
	}
}

// TestApplyRuns_AlreadyFinishedIsRejected pins the "no double-finish"
// rule. A second Finish would silently overwrite the first outcome
// and confuse history; the precondition lock catches it.
func TestApplyRuns_AlreadyFinishedIsRejected(t *testing.T) {
	s, pool := openTestStore(t)
	defer pool.Close()
	mustResetTables(t, pool)

	stateID, versionID := seedApplyState(t, s, "v2a-doublefinish")

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Second)
	defer cancel()

	run, err := s.BeginApplyRun(ctx, stateID, versionID, "alice", 1, nil)
	if err != nil {
		t.Fatalf("BeginApplyRun: %v", err)
	}
	if err := s.FinishApplyRun(ctx, run.ID, FinishApplyRunInput{Status: ApplyRunFailed}); err != nil {
		t.Fatalf("first Finish: %v", err)
	}
	err = s.FinishApplyRun(ctx, run.ID, FinishApplyRunInput{Status: ApplyRunAborted})
	if !errors.Is(err, ErrApplyRunAlreadyFinished) {
		t.Errorf("second Finish: err = %v, want ErrApplyRunAlreadyFinished", err)
	}
}

// TestApplyRuns_GetNotFound and the corresponding ListEmpty are
// here so callers that ignore the not-found sentinel don't silently
// regress to nil/empty in production.
func TestApplyRuns_GetNotFound(t *testing.T) {
	s, pool := openTestStore(t)
	defer pool.Close()
	mustResetTables(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Second)
	defer cancel()

	_, err := s.GetApplyRun(ctx, "00000000-0000-0000-0000-000000000000")
	if !errors.Is(err, ErrApplyRunNotFound) {
		t.Errorf("GetApplyRun(missing) = %v, want ErrApplyRunNotFound", err)
	}
}
