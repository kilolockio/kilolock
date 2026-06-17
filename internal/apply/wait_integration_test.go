//go:build integration

// Run with:
//
//	KL_DATABASE_URL=postgres://kl:kl@localhost:5432/kl?sslmode=disable \
//	  go test -tags=integration -run TestAcquireWithWait ./internal/apply/...
//
// Unlike the larger apply orchestrator tests, this file does NOT
// require terraform on PATH — it exercises only acquireWithWait
// against a live database, so it's safe to run in CI sandboxes
// without provider network access.

package apply

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/davesade/kilolock/internal/db"
	"github.com/davesade/kilolock/internal/migrate"
	"github.com/davesade/kilolock/internal/testdb"
	"github.com/davesade/kilolock/pkg/store"
)

// applyWaitRig is a lighter-weight cousin of applyTestRig: just the
// store and a freshly-seeded state, no fixture HCL, no terraform.
// Callers seed the state with a single trivial version so the FK
// from apply_runs.from_version_id has something to point at.
type applyWaitRig struct {
	store *store.Store
	pool  *db.Pool

	stateName string
	stateID   string
	versionID string
}

func newApplyWaitRig(t *testing.T, stateName string) *applyWaitRig {
	t.Helper()
	url := os.Getenv("KL_DATABASE_URL")
	if url == "" {
		url = os.Getenv("DATABASE_URL")
	}
	if url == "" {
		t.Skip("no KL_DATABASE_URL set")
	}

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 10*time.Second)
	defer cancel()

	pool, err := db.Open(ctx, url)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := migrate.Run(ctx, pool.Pool, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		pool.Close()
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { pool.Close() })

	// Wipe state (and dependent rows) from any previous run.
	if _, err := pool.Exec(ctx,
		`DELETE FROM states WHERE name = $1`, stateName,
	); err != nil {
		t.Fatalf("delete state %q: %v", stateName, err)
	}
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Second)
		defer ccancel()
		if _, err := pool.Exec(cctx,
			`DELETE FROM states WHERE name = $1`, stateName,
		); err != nil {
			t.Logf("post-test cleanup of state %q: %v", stateName, err)
		}
	})

	st := store.New(pool.Pool)

	// Seed a minimal state with one version. The apply_runs row
	// requires from_version_id, so a real version_id has to exist.
	body := map[string]any{
		"version":           4,
		"terraform_version": "1.13.4",
		"serial":            1,
		"lineage":           "44444444-4444-4444-4444-444444444444",
		"outputs":           map[string]any{},
		"resources":         []any{},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal seed: %v", err)
	}
	if err := st.WriteState(ctx, stateName, "", raw, "test", "tester"); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	info, err := st.GetCurrentStateInfo(ctx, stateName)
	if err != nil {
		t.Fatalf("GetCurrentStateInfo: %v", err)
	}
	return &applyWaitRig{
		store:     st,
		pool:      pool,
		stateName: stateName,
		stateID:   info.StateID,
		versionID: info.VersionID,
	}
}

// startApplyRun begins an apply_run and returns its ID. Helper so
// each test case can attach reservations to a real row.
func (r *applyWaitRig) startApplyRun(t *testing.T, actor string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Second)
	defer cancel()
	run, err := r.store.BeginApplyRun(ctx, r.stateID, r.versionID, actor, 1, nil)
	if err != nil {
		t.Fatalf("BeginApplyRun: %v", err)
	}
	return run.ID
}

// TestAcquireWithWait_NoWaitFailsFast is the regression-guard for
// the existing behaviour. With WaitForReservations=0, the helper
// MUST surface the conflict immediately without retrying.
func TestAcquireWithWait_NoWaitFailsFast(t *testing.T) {
	r := newApplyWaitRig(t, "wait-no-wait")

	// Alice's apply holds aws_instance.web.
	aliceApply := r.startApplyRun(t, "alice")
	ctxAlice, cancelAlice := context.WithTimeout(testdb.BackgroundTenantCtx(), 10*time.Second)
	defer cancelAlice()
	want := []store.Reservation{
		{AddressGlob: "aws_instance.web", Mode: store.ReservationWrite},
	}
	if err := r.store.AcquireReservations(ctxAlice, r.stateID, aliceApply, "alice", want, 5*time.Minute); err != nil {
		t.Fatalf("alice acquire: %v", err)
	}

	// Bob tries with WaitForReservations=0. Must error immediately.
	bobApply := r.startApplyRun(t, "bob")
	ctxBob, cancelBob := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Second)
	defer cancelBob()

	start := time.Now()
	err := acquireWithWait(ctxBob, r.store, r.stateID, bobApply, "bob",
		want, 5*time.Minute, Options{WaitForReservations: 0},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	elapsed := time.Since(start)

	if !errors.Is(err, store.ErrReservationConflict) {
		t.Errorf("err = %v, want ErrReservationConflict", err)
	}
	if elapsed > 1*time.Second {
		t.Errorf("fast-fail took %v, want < 1s", elapsed)
	}
}

// TestAcquireWithWait_WaitsThenSucceeds: alice's holder releases
// 1.5s into a 10s wait. The helper must retry and succeed,
// emitting at least one notifier event in the meantime.
func TestAcquireWithWait_WaitsThenSucceeds(t *testing.T) {
	r := newApplyWaitRig(t, "wait-then-success")

	aliceApply := r.startApplyRun(t, "alice")
	ctxAlice, cancelAlice := context.WithTimeout(testdb.BackgroundTenantCtx(), 10*time.Second)
	defer cancelAlice()
	want := []store.Reservation{
		{AddressGlob: "aws_instance.web", Mode: store.ReservationWrite},
	}
	if err := r.store.AcquireReservations(ctxAlice, r.stateID, aliceApply, "alice", want, 5*time.Minute); err != nil {
		t.Fatalf("alice acquire: %v", err)
	}

	// Release alice's reservation after 1.5s in a background goroutine.
	go func() {
		time.Sleep(1500 * time.Millisecond)
		ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Second)
		defer cancel()
		// nolint:errcheck — best-effort; test asserts on Bob's outcome
		_ = r.store.ReleaseReservations(ctx, aliceApply)
	}()

	var (
		mu     sync.Mutex
		events []WaitEvent
	)
	notify := func(ev WaitEvent) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, ev)
	}

	bobApply := r.startApplyRun(t, "bob")
	ctxBob, cancelBob := context.WithTimeout(testdb.BackgroundTenantCtx(), 10*time.Second)
	defer cancelBob()

	err := acquireWithWait(ctxBob, r.store, r.stateID, bobApply, "bob",
		want, 5*time.Minute, Options{
			WaitForReservations:     10 * time.Second,
			ReservationWaitNotifier: notify,
		},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("acquireWithWait: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(events) == 0 {
		t.Errorf("expected at least one wait notifier event")
	}
	// First event must carry the alice conflict.
	first := events[0]
	if len(first.Conflicts) != 1 || first.Conflicts[0].Holder != "alice" {
		t.Errorf("first event conflicts = %+v, want one row held by alice", first.Conflicts)
	}
}

// TestAcquireWithWait_BudgetExpires: alice never releases; bob's
// 2s wait expires; the helper returns ErrReservationConflict with
// the latest observed conflicts. The total elapsed should be
// approximately the wait budget — not radically over.
func TestAcquireWithWait_BudgetExpires(t *testing.T) {
	r := newApplyWaitRig(t, "wait-expires")

	aliceApply := r.startApplyRun(t, "alice")
	ctxAlice, cancelAlice := context.WithTimeout(testdb.BackgroundTenantCtx(), 10*time.Second)
	defer cancelAlice()
	want := []store.Reservation{
		{AddressGlob: "aws_instance.web", Mode: store.ReservationWrite},
	}
	if err := r.store.AcquireReservations(ctxAlice, r.stateID, aliceApply, "alice", want, 5*time.Minute); err != nil {
		t.Fatalf("alice acquire: %v", err)
	}

	bobApply := r.startApplyRun(t, "bob")
	ctxBob, cancelBob := context.WithTimeout(testdb.BackgroundTenantCtx(), 10*time.Second)
	defer cancelBob()

	start := time.Now()
	err := acquireWithWait(ctxBob, r.store, r.stateID, bobApply, "bob",
		want, 5*time.Minute, Options{
			WaitForReservations: 2 * time.Second,
		},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	elapsed := time.Since(start)

	if !errors.Is(err, store.ErrReservationConflict) {
		t.Errorf("err = %v, want ErrReservationConflict", err)
	}
	if elapsed < 2*time.Second {
		t.Errorf("expired after %v, want >= 2s", elapsed)
	}
	if elapsed > 4*time.Second {
		t.Errorf("expired after %v, want < 4s (helper should not greatly overrun budget)", elapsed)
	}
}

func TestRenewReservationLeases_StopsOnApplyAbort(t *testing.T) {
	r := newApplyWaitRig(t, "wait-heartbeat-abort")

	applyID := r.startApplyRun(t, "alice")
	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 10*time.Second)
	defer cancel()

	want := []store.Reservation{
		{AddressGlob: "aws_instance.web", Mode: store.ReservationWrite},
	}
	if err := r.store.AcquireReservations(ctx, r.stateID, applyID, "alice", want, 3*time.Second); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	go func() {
		time.Sleep(1200 * time.Millisecond)
		abortCtx, abortCancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Second)
		defer abortCancel()
		_ = r.store.AbortApplyRun(abortCtx, applyID, "aborted by test")
	}()

	err := renewReservationLeases(ctx, r.store, applyID, 3*time.Second, len(want), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		t.Fatal("expected heartbeat to stop on aborted apply")
	}
	if !strings.Contains(err.Error(), "apply aborted") {
		t.Fatalf("unexpected error: %v", err)
	}
}
