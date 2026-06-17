//go:build integration

// Pins the contract introduced by migration 0011: per-state
// optimistic locking. The acceptance set is:
//
//   1. Two operators hold concurrent locks against the same state.
//   2. Disjoint writes (different addresses) commit through both
//      WriteState calls without serial conflicts or lock errors.
//   3. Overlapping writes (same address touched by both) surface
//      a *WriteSetConflictError with the conflicting address
//      list and the post-conflict trunk serial.
//   4. states.exclusive_locks=true falls back to the legacy
//      one-writer-at-a-time semantics: AcquireLock returns
//      ErrAlreadyLocked while another lock is held.
//   5. A non-empty lineage mismatch always rejects (we never
//      merge across Terraform lineages).
//
// Run with:
//
//	KL_DATABASE_URL=postgres://kl:kl@localhost:5432/kl?sslmode=disable \
//	  go test -tags=integration -run TestOptimistic ./pkg/store/...

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/davesade/kilolock/internal/testdb"
)

// seedOptimisticStateRaw produces a parseable Terraform v4 state for
// the optimistic-merge tests. resources is a list of (type, name,
// attrJSON) triples; instances are single-instance, no index.
//
// The lineage is parameterized so the lineage-mismatch case can be
// expressed without a second helper.
func seedOptimisticStateRaw(t *testing.T, serial int, lineage string, resources ...[3]string) []byte {
	t.Helper()
	resList := make([]map[string]any, 0, len(resources))
	for _, r := range resources {
		typ, name, attrJSON := r[0], r[1], r[2]
		var attrs any
		if err := json.Unmarshal([]byte(attrJSON), &attrs); err != nil {
			t.Fatalf("seedOptimisticStateRaw: bad attr json for %s.%s: %v", typ, name, err)
		}
		resList = append(resList, map[string]any{
			"mode":     "managed",
			"type":     typ,
			"name":     name,
			"provider": `provider["registry.terraform.io/hashicorp/test"]`,
			"instances": []any{
				map[string]any{
					"schema_version": 0,
					"attributes":     attrs,
				},
			},
		})
	}
	body := map[string]any{
		"version":           4,
		"terraform_version": "1.6.0",
		"serial":            serial,
		"lineage":           lineage,
		"outputs":           map[string]any{},
		"resources":         resList,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal seed state: %v", err)
	}
	return raw
}

// makeLockInfo returns a LockInfo with a deterministic id derived
// from `tag`. Tests don't care about the rest of the fields, but
// state_locks has NOT NULL columns we must populate.
func makeLockInfo(tag string) LockInfo {
	return LockInfo{
		ID:        tag + "-lock-id",
		Operation: "OperationTypeApply",
		Info:      "",
		Who:       tag + "@host",
		Version:   "1.6.0",
		Created:   "2026-05-18T15:00:00Z",
		Path:      "qtest",
	}
}

// TestOptimistic_DisjointConcurrentWritesCommit drives the happy
// path Option A exists for: two operators acquire concurrent
// locks against the same state and POST disjoint writes; both
// commits succeed and the trunk ends with both changes.
func TestOptimistic_DisjointConcurrentWritesCommit(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	mustResetTables(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 15*time.Second)
	defer cancel()

	lineage := "9b39e2c0-aaaa-bbbb-cccc-000000000001"

	// Seed trunk at serial 1 with two resources.
	if err := s.WriteState(ctx, "qtest", "",
		seedOptimisticStateRaw(t, 1, lineage,
			[3]string{"aws_instance", "a", `{"v":1}`},
			[3]string{"aws_instance", "b", `{"v":1}`},
		),
		"test", "test",
	); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	// Operator A acquires a lock, observes trunk@serial=1.
	if _, err := s.AcquireLock(ctx, "qtest", makeLockInfo("alice")); err != nil {
		t.Fatalf("alice AcquireLock: %v", err)
	}
	// Operator B acquires a lock CONCURRENTLY against the same
	// state. With optimistic locking this is the difference: in
	// legacy mode this would error with ErrAlreadyLocked.
	if _, err := s.AcquireLock(ctx, "qtest", makeLockInfo("bob")); err != nil {
		t.Fatalf("bob AcquireLock (expected to succeed in optimistic mode): %v", err)
	}

	// Alice POSTs a state that only changes aws_instance.a.
	aliceState := seedOptimisticStateRaw(t, 2, lineage,
		[3]string{"aws_instance", "a", `{"v":2}`},
		[3]string{"aws_instance", "b", `{"v":1}`},
	)
	if err := s.WriteState(ctx, "qtest", makeLockInfo("alice").ID, aliceState, "alice", "alice"); err != nil {
		t.Fatalf("alice WriteState: %v", err)
	}

	// Bob POSTs a state that only changes aws_instance.b.
	// Bob's proposed.serial is 2 because Bob read trunk at serial 1
	// just like Alice did — but the backend recomputes the new
	// row's serial to MAX+1 = 3, transparently merging Alice's
	// already-committed change to "a".
	bobState := seedOptimisticStateRaw(t, 2, lineage,
		[3]string{"aws_instance", "a", `{"v":1}`}, // bob's stale view
		[3]string{"aws_instance", "b", `{"v":99}`},
	)
	if err := s.WriteState(ctx, "qtest", makeLockInfo("bob").ID, bobState, "bob", "bob"); err != nil {
		t.Fatalf("bob WriteState (expected merge to succeed): %v", err)
	}

	// Final trunk must carry both changes.
	raw, err := s.GetCurrentState(ctx, "qtest")
	if err != nil {
		t.Fatalf("GetCurrentState: %v", err)
	}
	if v, ok := resourceAttrValue(raw, "aws_instance", "a", "v"); !ok || v != 2 {
		t.Errorf("trunk aws_instance.a.v = %v (ok=%v), want 2: %s", v, ok, raw)
	}
	if v, ok := resourceAttrValue(raw, "aws_instance", "b", "v"); !ok || v != 99 {
		t.Errorf("trunk aws_instance.b.v = %v (ok=%v), want 99: %s", v, ok, raw)
	}

	// Lock rows are NOT consumed on commit — vanilla Terraform
	// always pairs LOCK with UNLOCK and the UNLOCK would 409
	// otherwise. The two rows should still be present.
	var lockCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM state_locks
		 WHERE state_id = (SELECT id FROM states WHERE name = 'qtest')`,
	).Scan(&lockCount); err != nil {
		t.Fatalf("count state_locks: %v", err)
	}
	if lockCount != 2 {
		t.Errorf("state_locks rows after dual commit = %d, want 2 (locks survive until UNLOCK)", lockCount)
	}

	// Both operators can release their own lock independently —
	// the per-lock-id targeted DELETE in ReleaseLock plays nicely
	// with multiple holders.
	if err := s.ReleaseLock(ctx, "qtest", makeLockInfo("alice").ID, "alice"); err != nil {
		t.Errorf("alice ReleaseLock: %v", err)
	}
	if err := s.ReleaseLock(ctx, "qtest", makeLockInfo("bob").ID, "bob"); err != nil {
		t.Errorf("bob ReleaseLock: %v", err)
	}
}

// TestOptimistic_OverlappingWritesConflict drives the rejection
// path: two operators touch the same address; the second POST
// returns a typed conflict that names the address.
func TestOptimistic_OverlappingWritesConflict(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	mustResetTables(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 15*time.Second)
	defer cancel()

	lineage := "9b39e2c0-aaaa-bbbb-cccc-000000000002"

	if err := s.WriteState(ctx, "qtest", "",
		seedOptimisticStateRaw(t, 1, lineage,
			[3]string{"aws_instance", "shared", `{"v":1}`},
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

	// Alice commits a change to "shared". Trunk advances to
	// serial 2 with shared.v=2.
	if err := s.WriteState(ctx, "qtest", makeLockInfo("alice").ID,
		seedOptimisticStateRaw(t, 2, lineage,
			[3]string{"aws_instance", "shared", `{"v":2}`},
		),
		"alice", "alice",
	); err != nil {
		t.Fatalf("alice WriteState: %v", err)
	}

	// Bob also tries to commit a change to "shared". Conflict.
	err := s.WriteState(ctx, "qtest", makeLockInfo("bob").ID,
		seedOptimisticStateRaw(t, 2, lineage,
			[3]string{"aws_instance", "shared", `{"v":3}`},
		),
		"bob", "bob",
	)
	var conflict *WriteSetConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("bob WriteState: want *WriteSetConflictError, got %v", err)
	}
	if len(conflict.Addresses) != 1 || conflict.Addresses[0] != "aws_instance.shared" {
		t.Errorf("conflict.Addresses = %v, want [aws_instance.shared]", conflict.Addresses)
	}
	if conflict.LatestSerial != 2 {
		t.Errorf("conflict.LatestSerial = %d, want 2", conflict.LatestSerial)
	}

	// Bob's failed commit must NOT consume his lock row — leaving
	// his subsequent retry able to commit against the new trunk
	// without re-locking.
	var bobLockRows int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM state_locks
		 WHERE state_id = (SELECT id FROM states WHERE name = 'qtest')
		   AND lock_id = $1`,
		makeLockInfo("bob").ID,
	).Scan(&bobLockRows); err != nil {
		t.Fatalf("count bob lock rows: %v", err)
	}
	if bobLockRows != 1 {
		t.Errorf("bob lock rows after conflict = %d, want 1 (lock preserved on failed commit)", bobLockRows)
	}

	// Trunk's value for "shared" is alice's, not bob's.
	raw, err := s.GetCurrentState(ctx, "qtest")
	if err != nil {
		t.Fatalf("GetCurrentState: %v", err)
	}
	if v, ok := resourceAttrValue(raw, "aws_instance", "shared", "v"); !ok || v != 2 {
		t.Errorf("trunk aws_instance.shared.v = %v (ok=%v), want 2 (alice's): %s", v, ok, raw)
	}
}

// TestOptimistic_SameLockCanCommitSequentialWrites verifies the lock baseline
// advances after each successful write, so a long-running Terraform apply or
// destroy does not conflict with its own earlier checkpoint writes.
func TestOptimistic_SameLockCanCommitSequentialWrites(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	mustResetTables(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 15*time.Second)
	defer cancel()

	lineage := "9b39e2c0-aaaa-bbbb-cccc-000000000006"
	lock := makeLockInfo("alice")

	if err := s.WriteState(ctx, "qtest", "",
		seedOptimisticStateRaw(t, 1, lineage,
			[3]string{"aws_instance", "shared", `{"v":1}`},
		),
		"test", "test",
	); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	if _, err := s.AcquireLock(ctx, "qtest", lock); err != nil {
		t.Fatalf("alice AcquireLock: %v", err)
	}

	if err := s.WriteState(ctx, "qtest", lock.ID,
		seedOptimisticStateRaw(t, 2, lineage,
			[3]string{"aws_instance", "shared", `{"v":2}`},
		),
		"alice", "alice",
	); err != nil {
		t.Fatalf("alice first WriteState: %v", err)
	}

	// Simulate a later checkpoint from the same long-running apply/destroy:
	// same held lock, overlapping address, and a state payload that was built
	// from the operator's own next step rather than from a fresh lock acquire.
	if err := s.WriteState(ctx, "qtest", lock.ID,
		seedOptimisticStateRaw(t, 2, lineage,
			[3]string{"aws_instance", "shared", `{"v":3}`},
		),
		"alice", "alice",
	); err != nil {
		t.Fatalf("alice second WriteState with same lock should not self-conflict: %v", err)
	}

	raw, err := s.GetCurrentState(ctx, "qtest")
	if err != nil {
		t.Fatalf("GetCurrentState: %v", err)
	}
	if v, ok := resourceAttrValue(raw, "aws_instance", "shared", "v"); !ok || v != 3 {
		t.Errorf("trunk aws_instance.shared.v = %v (ok=%v), want 3: %s", v, ok, raw)
	}

	var sourceSerial int64
	if err := pool.QueryRow(ctx,
		`SELECT source_serial FROM state_locks
		 WHERE state_id = (SELECT id FROM states WHERE name = 'qtest')
		   AND lock_id = $1`,
		lock.ID,
	).Scan(&sourceSerial); err != nil {
		t.Fatalf("read updated lock baseline: %v", err)
	}
	if sourceSerial != 3 {
		t.Errorf("source_serial after sequential writes = %d, want 3", sourceSerial)
	}
}

// TestOptimistic_ExclusiveLocksFlagFallsBackToLegacy verifies the
// per-state escape hatch: with exclusive_locks=true the second
// AcquireLock returns ErrAlreadyLocked just like pre-0011.
func TestOptimistic_ExclusiveLocksFlagFallsBackToLegacy(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	mustResetTables(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 15*time.Second)
	defer cancel()

	lineage := "9b39e2c0-aaaa-bbbb-cccc-000000000003"

	if err := s.WriteState(ctx, "qtest", "",
		seedOptimisticStateRaw(t, 1, lineage,
			[3]string{"aws_instance", "x", `{"v":1}`},
		),
		"test", "test",
	); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	// Operator toggle: simulate an admin flipping this state into
	// exclusive mode.
	if _, err := pool.Exec(ctx, `UPDATE states SET exclusive_locks = true WHERE name = 'qtest'`); err != nil {
		t.Fatalf("flip exclusive_locks: %v", err)
	}

	if _, err := s.AcquireLock(ctx, "qtest", makeLockInfo("alice")); err != nil {
		t.Fatalf("alice AcquireLock: %v", err)
	}
	// Bob now MUST be rejected — this is the whole point of the
	// exclusive_locks toggle.
	_, err := s.AcquireLock(ctx, "qtest", makeLockInfo("bob"))
	if !errors.Is(err, ErrAlreadyLocked) {
		t.Fatalf("bob AcquireLock with exclusive_locks=true: want ErrAlreadyLocked, got %v", err)
	}
}

// TestOptimistic_LineageMismatchAlwaysRejects verifies the
// safety-rail: even in optimistic mode, we never merge across
// Terraform lineages. The proposed state's lineage differs from
// trunk's → ErrLineageMismatch.
func TestOptimistic_LineageMismatchAlwaysRejects(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	mustResetTables(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 15*time.Second)
	defer cancel()

	lineage1 := "9b39e2c0-aaaa-bbbb-cccc-000000000004"
	lineage2 := "9b39e2c0-aaaa-bbbb-cccc-000000000005"

	if err := s.WriteState(ctx, "qtest", "",
		seedOptimisticStateRaw(t, 1, lineage1,
			[3]string{"aws_instance", "a", `{"v":1}`},
		),
		"test", "test",
	); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	if _, err := s.AcquireLock(ctx, "qtest", makeLockInfo("alice")); err != nil {
		t.Fatalf("alice AcquireLock: %v", err)
	}
	// Concurrent committer changes "a", trunk advances to L1.
	if _, err := s.AcquireLock(ctx, "qtest", makeLockInfo("bob")); err != nil {
		t.Fatalf("bob AcquireLock: %v", err)
	}
	if err := s.WriteState(ctx, "qtest", makeLockInfo("bob").ID,
		seedOptimisticStateRaw(t, 2, lineage1,
			[3]string{"aws_instance", "a", `{"v":2}`},
		),
		"bob", "bob",
	); err != nil {
		t.Fatalf("bob WriteState: %v", err)
	}

	// Alice now POSTs a state with a DIFFERENT lineage. With
	// trunk's lineage = L1 and proposed = L2 the merge must
	// reject regardless of write-set disjointness.
	err := s.WriteState(ctx, "qtest", makeLockInfo("alice").ID,
		seedOptimisticStateRaw(t, 2, lineage2,
			[3]string{"aws_instance", "z", `{"v":1}`},
		),
		"alice", "alice",
	)
	if !errors.Is(err, ErrLineageMismatch) {
		t.Fatalf("alice WriteState with wrong lineage: want ErrLineageMismatch, got %v", err)
	}
}

// resourceAttrValue extracts the integer value of attribute `key`
// from the named resource's single-instance attributes blob. Used
// to assert merged outputs without depending on PostgreSQL's jsonb
// re-formatting (which inserts whitespace differently from the
// test inputs).
//
// Returns ok=false when the resource (or attribute key) is absent.
func resourceAttrValue(raw []byte, typ, name, key string) (float64, bool) {
	var doc struct {
		Resources []struct {
			Type      string `json:"type"`
			Name      string `json:"name"`
			Instances []struct {
				Attributes map[string]any `json:"attributes"`
			} `json:"instances"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return 0, false
	}
	target := fmt.Sprintf("%s.%s", typ, name)
	for _, r := range doc.Resources {
		if fmt.Sprintf("%s.%s", r.Type, r.Name) != target {
			continue
		}
		for _, inst := range r.Instances {
			v, ok := inst.Attributes[key]
			if !ok {
				continue
			}
			f, ok := v.(float64)
			if !ok {
				return 0, false
			}
			return f, true
		}
	}
	return 0, false
}
