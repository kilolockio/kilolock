//go:build integration

// Pins the contract of WriteStateForApply against the v1 HTTP-backend
// state lock. The v2 apply orchestrator must NOT be blocked by
// state_locks rows, otherwise a single leaked terraform-side lock
// (a SIGKILLed `terraform plan`, for example) bricks every
// kl apply on that state until an operator manually
// DELETEs the row. See ADR 0007 and the comment on
// Store.WriteStateForApply.
//
// Run with:
//
//	KL_DATABASE_URL=postgres://kl:kl@localhost:5432/kl?sslmode=disable \
//	  go test -tags=integration -run TestWriteStateForApply ./pkg/store/...

package store

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/kilolockio/kilolock/pkg/testdb"
	"testing"
	"time"
)

// seedQtestStateRaw produces a minimal but parseable Terraform v4
// state document for state name "qtest". serial is parameterized so
// individual subtests can write monotonic versions without colliding.
func seedQtestStateRaw(t *testing.T, serial int) []byte {
	t.Helper()
	body := map[string]any{
		"version":           4,
		"terraform_version": "1.13.4",
		"serial":            serial,
		"lineage":           "9b39e2c0-aaaa-bbbb-cccc-444455556666",
		"outputs":           map[string]any{},
		"resources":         []any{},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal seed state: %v", err)
	}
	return raw
}

// TestWriteStateForApply_IgnoresStateLock verifies the bug fix: a
// stale terraform-side lock on state_locks does NOT prevent the v2
// apply orchestrator from committing a new state version, while the
// v1 WriteState path (vanilla terraform clients) still correctly
// fails with ErrStateLocked. Both halves are asserted in one test
// because they're the two sides of the same contract and drift apart
// silently if separated.
func TestWriteStateForApply_IgnoresStateLock(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	mustResetTables(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 10*time.Second)
	defer cancel()

	// 1. Seed an initial state version via the v1 path with no lock.
	//    This creates the states row that the lock-acquire below can
	//    bind to.
	if err := s.WriteState(ctx, "qtest", "", seedQtestStateRaw(t, 1), "test", "test"); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	// 2. Simulate a leaked terraform-side lock by acquiring it and
	//    never releasing. This is exactly the "SIGKILLed terraform
	//    plan" failure mode we hit during the v2d demo.
	leaked := LockInfo{
		ID:        "leaked-lock-from-killed-terraform-plan",
		Operation: "OperationTypePlan",
		Info:      "",
		Who:       "test@host",
		Version:   "1.13.4",
		Created:   "2026-05-14T14:00:00Z",
		Path:      "qtest",
	}
	if _, err := s.AcquireLock(ctx, "qtest", leaked); err != nil {
		t.Fatalf("acquire leaked lock: %v", err)
	}

	// 3. v1 path with no lock id MUST still be rejected — this is
	//    the vanilla-terraform-client contract we preserve so that
	//    coexistence with vanilla terraform remains safe until v2e
	//    models it as a *-glob reservation.
	err := s.WriteState(ctx, "qtest", "", seedQtestStateRaw(t, 2), "test", "test")
	if !errors.Is(err, ErrStateLocked) {
		t.Fatalf("v1 WriteState with leaked lock: expected ErrStateLocked, got %v", err)
	}

	// 4. v2 path MUST succeed despite the leaked lock — this is the
	//    bug we're fixing. The orchestrator holds row-level
	//    reservations and explicitly opts out of the v1 whole-state
	//    lock.
	if err := s.WriteStateForApply(ctx, "qtest", "apply-1", 1, seedQtestStateRaw(t, 3), "apply", "alice"); err != nil {
		t.Fatalf("WriteStateForApply with leaked lock: expected success, got %v", err)
	}

	// 5. The serial-uniqueness invariant must still hold inside the
	//    apply path. If two orchestrators race to commit the same
	//    serial, the second sees ErrSerialConflict (not silent
	//    overwrite). This is the only safety net left after we
	//    bypass state_locks; if it ever breaks we'd corrupt trunk.
	err = s.WriteStateForApply(ctx, "qtest", "apply-2", 1, seedQtestStateRaw(t, 3), "apply", "bob")
	if !errors.Is(err, ErrSerialConflict) {
		t.Fatalf("WriteStateForApply with duplicate serial: expected ErrSerialConflict, got %v", err)
	}

	// 6. A higher serial via the apply path is fine even with the
	//    leaked lock still in place — confirming the bypass is not
	//    conditional on anything in the lock row.
	if err := s.WriteStateForApply(ctx, "qtest", "apply-3", 3, seedQtestStateRaw(t, 4), "apply", "alice"); err != nil {
		t.Fatalf("WriteStateForApply with new serial: %v", err)
	}

	// 7. Sanity: the lock row is still there. The orchestrator did
	//    not silently mutate state_locks.
	var lockID string
	err = pool.QueryRow(ctx,
		`SELECT lock_id FROM state_locks
		 WHERE state_id = (SELECT id FROM states WHERE name = 'qtest')`,
	).Scan(&lockID)
	if err != nil {
		t.Fatalf("read state_lock after apply: %v", err)
	}
	if lockID != leaked.ID {
		t.Fatalf("state_lock mutated by apply path: got %q want %q", lockID, leaked.ID)
	}
}
