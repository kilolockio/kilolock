//go:build integration

// Integration tests for the v2c-1 sliced-apply orchestrator.
//
// Run with:
//
//	KL_DATABASE_URL=postgres://kl:kl@localhost:5432/kl?sslmode=disable \
//	  go test -tags=integration -run TestApplyOrchestrator ./internal/apply/...
//
// Requires:
//   - A reachable Postgres reachable via KL_DATABASE_URL (or DATABASE_URL).
//   - A `terraform` binary on PATH (>= 1.4, for the `terraform_data` builtin).
//   - Network access for `terraform init` to set up its embedded
//     provider registry — `terraform_data` is built-in but init
//     still consults the registry for cache invalidation. If your
//     CI sandbox blocks this, run these tests with -count=0 to skip.
//
// The fixture is a two-resource module using `terraform_data`, which
// has no provider dependency so the apply tmp dir's `terraform init`
// is hermetic.

package apply

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kilolockio/kilolock/internal/plan"
	"github.com/kilolockio/kilolock/pkg/db"
	"github.com/kilolockio/kilolock/pkg/migrate"
	"github.com/kilolockio/kilolock/pkg/store"
	"github.com/kilolockio/kilolock/pkg/testdb"
)

// applyTestRig bundles the per-test infrastructure: a fresh Store,
// the underlying pool (so the test can TRUNCATE), the path to
// terraform, the HCL fixture's working directory, and the
// PlanSpec to feed apply.Run. Tests assemble this with newApplyRig
// and tear it down via t.Cleanup.
type applyTestRig struct {
	store        *store.Store
	pool         *db.Pool
	terraformBin string
	workDir      string
	spec         *plan.PlanSpec
	stateName    string
}

func newApplyRig(t *testing.T, stateName string) *applyTestRig {
	t.Helper()

	url := os.Getenv("KL_DATABASE_URL")
	if url == "" {
		url = os.Getenv("DATABASE_URL")
	}
	if url == "" {
		t.Skip("no KL_DATABASE_URL or DATABASE_URL set")
	}
	tfBin, err := exec.LookPath("terraform")
	if err != nil {
		t.Skipf("terraform not on PATH: %v", err)
	}

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 15*time.Second)
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

	// Targeted cleanup: remove only the state row this test will
	// write, plus any apply_runs / reservations attached to it,
	// plus any apply_runs that target a state row matching our
	// test-name prefix. This intentionally avoids TRUNCATE so a
	// developer running the integration suite on a workstation
	// can't accidentally wipe their own dev states (big-state,
	// drift-demo, etc.) the way an unscoped TRUNCATE would.
	if _, err := pool.Exec(ctx,
		`DELETE FROM apply_runs WHERE state_id IN (
		   SELECT id FROM states WHERE name = $1
		 )`, stateName,
	); err != nil {
		t.Fatalf("delete apply_runs for %q: %v", stateName, err)
	}
	if _, err := pool.Exec(ctx,
		`DELETE FROM states WHERE name = $1`, stateName,
	); err != nil {
		t.Fatalf("delete state %q: %v", stateName, err)
	}
	t.Cleanup(func() {
		// Best-effort post-run cleanup so each test leaves the
		// DB in the shape it found it. Failures here are warnings
		// rather than fatals — a leftover row from a failed test
		// is debuggable; a fatal would mask the actual test error.
		cctx, ccancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Second)
		defer ccancel()
		if _, err := pool.Exec(cctx,
			`DELETE FROM states WHERE name = $1`, stateName,
		); err != nil {
			t.Logf("post-test cleanup of state %q: %v", stateName, err)
		}
	})

	st := store.New(pool.Pool)
	workDir := t.TempDir()

	return &applyTestRig{
		store:        st,
		pool:         pool,
		terraformBin: tfBin,
		workDir:      workDir,
		stateName:    stateName,
	}
}

// twoResourceFixture writes a 2-resource HCL fixture into r.workDir
// AND seeds the kl trunk with a state that matches the
// "before" values.
//
// Real-state seeding strategy: instead of hand-crafting a
// terraform_data state JSON (which has a fussy dynamic-type wire
// encoding that requires every field to be in cty form), we run
// `terraform apply` once in a separate bootstrap directory with
// the BEFORE values, read the resulting terraform.tfstate, and
// import that into kl. Then we overwrite the OPERATOR's
// HCL with the AFTER values. From the orchestrator's perspective,
// the trunk holds a real-shaped state at the BEFORE values, and
// the operator's HCL describes the AFTER values — a textbook
// in-place update.
//
// Cost: one extra `terraform init && terraform apply` per test
// case. Slow-ish but the alternative (a committed binary state
// fixture, version-coupled to terraform) is worse.
func (r *applyTestRig) twoResourceFixture(t *testing.T, beforeA, beforeB, afterA, afterB string) {
	t.Helper()

	hcl := func(a, b string) string {
		return fmt.Sprintf(`terraform {
  required_version = ">= 1.4.0"
}

resource "terraform_data" "a" {
  input = "%s"
}

resource "terraform_data" "b" {
  input = "%s"
}
`, a, b)
	}

	// Bootstrap dir holds the BEFORE HCL; we apply it to disk to
	// get a real terraform_data state, then read it back. The
	// bootstrap directory is independent of r.workDir so its
	// .terraform/ and .terraform.lock.hcl don't leak into the
	// apply orchestrator's input.
	bootDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(bootDir, "main.tf"), []byte(hcl(beforeA, beforeB)), 0o644); err != nil {
		t.Fatalf("write bootstrap main.tf: %v", err)
	}

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 2*time.Minute)
	defer cancel()

	for _, args := range [][]string{
		{"init", "-input=false", "-no-color"},
		{"apply", "-auto-approve", "-input=false", "-no-color"},
	} {
		cmd := exec.CommandContext(ctx, r.terraformBin, args...)
		cmd.Dir = bootDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("bootstrap terraform %v: %v\n%s", args, err, out)
		}
	}

	stateBytes, err := os.ReadFile(filepath.Join(bootDir, "terraform.tfstate"))
	if err != nil {
		t.Fatalf("read bootstrap state: %v", err)
	}

	// Operator's work directory carries the AFTER HCL. The trunk
	// (just imported) carries the BEFORE values. terraform_data.a
	// is the address whose input is changing.
	if err := os.WriteFile(filepath.Join(r.workDir, "main.tf"), []byte(hcl(afterA, afterB)), 0o644); err != nil {
		t.Fatalf("write work main.tf: %v", err)
	}

	// Import the bootstrap state into kl as the trunk.
	wctx, wcancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 10*time.Second)
	defer wcancel()
	if err := r.store.WriteState(wctx, r.stateName, "", stateBytes, "import", "test"); err != nil {
		t.Fatalf("seed initial state: %v", err)
	}
}

func (r *applyTestRig) slowSleepFixture(t *testing.T, beforeDuration, afterDuration string) {
	t.Helper()

	hcl := func(d string) string {
		return fmt.Sprintf(`terraform {
  required_version = ">= 1.4.0"
  required_providers {
    time = {
      source  = "hashicorp/time"
      version = ">= 0.9.0"
    }
  }
}

provider "time" {}

resource "time_sleep" "slow" {
  create_duration = "%s"
}
`, d)
	}

	bootDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(bootDir, "main.tf"), []byte(hcl(beforeDuration)), 0o644); err != nil {
		t.Fatalf("write bootstrap main.tf: %v", err)
	}

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 3*time.Minute)
	defer cancel()

	for _, args := range [][]string{
		{"init", "-input=false", "-no-color"},
		{"apply", "-auto-approve", "-input=false", "-no-color"},
	} {
		cmd := exec.CommandContext(ctx, r.terraformBin, args...)
		cmd.Dir = bootDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("bootstrap terraform %v: %v\n%s", args, err, out)
		}
	}

	stateBytes, err := os.ReadFile(filepath.Join(bootDir, "terraform.tfstate"))
	if err != nil {
		t.Fatalf("read bootstrap state: %v", err)
	}

	if err := os.WriteFile(filepath.Join(r.workDir, "main.tf"), []byte(hcl(afterDuration)), 0o644); err != nil {
		t.Fatalf("write work main.tf: %v", err)
	}

	wctx, wcancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 10*time.Second)
	defer wcancel()
	if err := r.store.WriteState(wctx, r.stateName, "", stateBytes, "import", "test"); err != nil {
		t.Fatalf("seed initial state: %v", err)
	}
}

// handCraftPlanSpec builds a minimal v2b-shaped PlanSpec covering
// the two-resource fixture. Production code derives this from
// `terraform show -json`; for a focused integration test we skip
// that step and just describe directly what we want apply.Run to do.
func handCraftPlanSpec(t *testing.T, configDir, writeAddr string) *plan.PlanSpec {
	t.Helper()
	return &plan.PlanSpec{
		FormatVersion: plan.CurrentSpecFormatVersion,
		GeneratedAt:   time.Now().UTC(),
		ConfigDir:     configDir,
		PlanSummary: plan.PlanSummary{
			Update: 1,
		},
		WriteSet: []string{writeAddr},
		ReadSet:  []string{writeAddr},
		HCLFootprint: []string{
			"terraform_data.a",
			"terraform_data.b",
		},
		Reservations: []plan.PlanReservation{
			{Address: writeAddr, Mode: "write"},
		},
	}
}

func handCraftSingleAddrPlanSpec(t *testing.T, configDir, writeAddr string) *plan.PlanSpec {
	t.Helper()
	return &plan.PlanSpec{
		FormatVersion: plan.CurrentSpecFormatVersion,
		GeneratedAt:   time.Now().UTC(),
		ConfigDir:     configDir,
		PlanSummary: plan.PlanSummary{
			Replace: 1,
		},
		WriteSet:     []string{writeAddr},
		ReadSet:      []string{writeAddr},
		HCLFootprint: []string{writeAddr},
		Reservations: []plan.PlanReservation{
			{Address: writeAddr, Mode: "write"},
		},
	}
}

// TestApplyOrchestrator_EndToEnd_HappyPath is the v2c-1 smoke
// test: seed a state, drive an apply that mutates one of the two
// resources, and assert all the visible side effects:
//
//   - apply_runs row is status='committed' with the right counters
//   - a new state_versions row exists at serial+1 with source='apply'
//   - the post-apply trunk's row for the mutated address shows the
//     new input value
//   - the non-write-set row is byte-identical to its pre-apply trunk
//   - resource_reservations is empty (released after the run)
func TestApplyOrchestrator_EndToEnd_HappyPath(t *testing.T) {
	stateName := "apply-it-happy"
	rig := newApplyRig(t, stateName)
	rig.twoResourceFixture(t,
		"old-a", "old-b", // before
		"new-a", "old-b", // after: change A, leave B alone
	)
	spec := handCraftPlanSpec(t, rig.workDir, "terraform_data.a")

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Minute)
	defer cancel()

	res, err := Run(ctx, rig.store, Options{
		Spec:         spec,
		StateName:    stateName,
		Actor:        "integration-test",
		WorkDir:      rig.workDir,
		TerraformBin: rig.terraformBin,
		Lease:        5 * time.Minute,
		SkipCleanup:  false,
		NoColor:      true,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("apply.Run: %v\nresult: %+v", err, res)
	}
	if res == nil {
		t.Fatal("apply.Run returned nil result")
	}
	// The committed serial is the bootstrap-import's serial + 1.
	// Terraform's bootstrap may finish at serial 1, 2, or 3 depending
	// on whether the local-backend writes intermediate plan/refresh
	// state versions, so we assert the relative bump rather than an
	// absolute value.
	if res.CommittedSerial != res.SourceSerial+1 {
		t.Errorf("committed serial: got %d want %d (source+1)",
			res.CommittedSerial, res.SourceSerial+1)
	}
	if res.ResourcesApplied != 1 {
		t.Errorf("resources applied: got %d want 1 (terraform_data.a only)", res.ResourcesApplied)
	}
	if got := res.AppliedAddresses; !(len(got) == 1 && got[0] == "terraform_data.a") {
		t.Errorf("applied addresses: got %v want [terraform_data.a]", got)
	}

	var status string
	var committed *int64
	if err := rig.pool.QueryRow(ctx,
		`SELECT status, committed_serial FROM apply_runs WHERE id = $1`,
		res.ApplyID,
	).Scan(&status, &committed); err != nil {
		t.Fatalf("query apply_runs: %v", err)
	}
	if status != "committed" {
		t.Errorf("apply_runs.status: got %q want %q", status, "committed")
	}
	if committed == nil || *committed != res.CommittedSerial {
		got := int64(-1)
		if committed != nil {
			got = *committed
		}
		t.Errorf("apply_runs.committed_serial: got %d want %d", got, res.CommittedSerial)
	}

	// Reservations released.
	var rcount int
	if err := rig.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM resource_reservations WHERE apply_id = $1`,
		res.ApplyID,
	).Scan(&rcount); err != nil {
		t.Fatalf("query reservations: %v", err)
	}
	if rcount != 0 {
		t.Errorf("expected reservations cleaned up, got %d remaining", rcount)
	}

	// Verify the trunk now reflects new-a for a and old-b for b.
	raw, err := rig.store.GetCurrentState(ctx, stateName)
	if err != nil {
		t.Fatalf("fetch new trunk: %v", err)
	}
	var snap map[string]any
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatalf("parse new trunk: %v", err)
	}
	got := extractInputs(t, snap)
	if got["terraform_data.a"] != "new-a" {
		t.Errorf("post-apply terraform_data.a.input: got %q want %q", got["terraform_data.a"], "new-a")
	}
	if got["terraform_data.b"] != "old-b" {
		t.Errorf("non-write_set terraform_data.b.input must be preserved: got %q want %q",
			got["terraform_data.b"], "old-b")
	}
}

func TestApplyOrchestrator_GenesisStateCreatedOnFirstApply(t *testing.T) {
	stateName := "apply-it-genesis"
	rig := newApplyRig(t, stateName)

	const hcl = `terraform {
  required_version = ">= 1.4.0"
}

resource "terraform_data" "a" {
  input = "genesis-a"
}
`
	if err := os.WriteFile(filepath.Join(rig.workDir, "main.tf"), []byte(hcl), 0o644); err != nil {
		t.Fatalf("write work main.tf: %v", err)
	}

	spec := &plan.PlanSpec{
		FormatVersion: plan.CurrentSpecFormatVersion,
		GeneratedAt:   time.Now().UTC(),
		ConfigDir:     rig.workDir,
		PlanSummary: plan.PlanSummary{
			Create: 1,
		},
		WriteSet:     []string{"terraform_data.a"},
		ReadSet:      []string{"terraform_data.a"},
		HCLFootprint: []string{"terraform_data.a"},
		Reservations: []plan.PlanReservation{
			{Address: "terraform_data.a", Mode: "write"},
		},
	}

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Minute)
	defer cancel()

	res, err := Run(ctx, rig.store, Options{
		Spec:         spec,
		StateName:    stateName,
		Actor:        "integration-test",
		WorkDir:      rig.workDir,
		TerraformBin: rig.terraformBin,
		Lease:        5 * time.Minute,
		SkipCleanup:  false,
		NoColor:      true,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("apply.Run genesis: %v\nresult: %+v", err, res)
	}
	if res == nil {
		t.Fatal("apply.Run returned nil result")
	}
	if res.SourceSerial != 0 {
		t.Fatalf("source serial=%d want 0 for genesis apply", res.SourceSerial)
	}
	if res.CommittedSerial != 1 {
		t.Fatalf("committed serial=%d want 1 for first real version", res.CommittedSerial)
	}
	if res.ResourcesApplied != 1 {
		t.Fatalf("resources applied=%d want 1", res.ResourcesApplied)
	}

	info, err := rig.store.GetCurrentStateInfo(ctx, stateName)
	if err != nil {
		t.Fatalf("GetCurrentStateInfo: %v", err)
	}
	if info.Serial != 1 {
		t.Fatalf("current serial=%d want 1", info.Serial)
	}

	raw, err := rig.store.GetCurrentState(ctx, stateName)
	if err != nil {
		t.Fatalf("fetch current state: %v", err)
	}
	var snap map[string]any
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatalf("parse current state: %v", err)
	}
	got := extractInputs(t, snap)
	if got["terraform_data.a"] != "genesis-a" {
		t.Fatalf("terraform_data.a.input=%q want %q", got["terraform_data.a"], "genesis-a")
	}
}

func TestApplyOrchestrator_HeartbeatRenewsReservations(t *testing.T) {
	stateName := "apply-it-heartbeat"
	rig := newApplyRig(t, stateName)
	rig.slowSleepFixture(t, "0s", "5s")

	spec := handCraftSingleAddrPlanSpec(t, rig.workDir, "time_sleep.slow")

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Minute)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		_, err := Run(ctx, rig.store, Options{
			Spec:         spec,
			StateName:    stateName,
			Actor:        "integration-test",
			WorkDir:      rig.workDir,
			TerraformBin: rig.terraformBin,
			Lease:        2 * time.Second,
			SkipCleanup:  false,
			NoColor:      true,
		}, slog.New(slog.NewTextHandler(io.Discard, nil)))
		errCh <- err
	}()

	deadline := time.Now().Add(5 * time.Second)
	var oldExpiry time.Time
	for {
		active, err := rig.store.ListActiveReservations(ctx, stateName)
		if err != nil {
			t.Fatalf("list active reservations: %v", err)
		}
		if len(active) > 0 {
			oldExpiry = active[0].ExpiresAt
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for initial reservations")
		}
		time.Sleep(50 * time.Millisecond)
	}

	info, err := rig.store.GetCurrentStateInfo(ctx, stateName)
	if err != nil {
		t.Fatalf("read trunk info: %v", err)
	}
	conflictObserved := false
	deadline = oldExpiry.Add(4 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-errCh:
			if err != nil {
				t.Fatalf("apply.Run: %v", err)
			}
			if !conflictObserved {
				t.Fatalf("apply finished before reservation heartbeat conflict check succeeded")
			}
			return
		default:
		}

		attacker, err := rig.store.BeginApplyRun(ctx, info.StateID, info.VersionID, "attacker", info.Serial, nil)
		if err != nil {
			t.Fatalf("begin attacker apply_run: %v", err)
		}

		acqErr := rig.store.AcquireReservations(ctx, info.StateID, attacker.ID, "attacker", []store.Reservation{
			{AddressGlob: "time_sleep.slow", Mode: store.ReservationWrite},
		}, 30*time.Second)
		if acqErr != nil {
			var conflict *store.ReservationConflictError
			if errors.As(acqErr, &conflict) {
				conflictObserved = true
				break
			}
			t.Fatalf("expected ReservationConflictError, got %T: %v", acqErr, acqErr)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !conflictObserved {
		t.Fatalf("timed out waiting for reservation conflict after initial expiry=%v", oldExpiry)
	}

	if err := <-errCh; err != nil {
		t.Fatalf("apply.Run: %v", err)
	}
}

func TestApplyOrchestrator_StateEngineCoarseLock_BlocksVanillaTerraformAndIsReleased(t *testing.T) {
	stateName := "apply-it-state-engine-lock"
	rig := newApplyRig(t, stateName)
	rig.slowSleepFixture(t, "0s", "5s")

	spec := handCraftSingleAddrPlanSpec(t, rig.workDir, "time_sleep.slow")

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Minute)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		_, err := Run(ctx, rig.store, Options{
			Spec:               spec,
			StateName:          stateName,
			Actor:              "integration-test",
			WorkDir:            rig.workDir,
			TerraformBin:       rig.terraformBin,
			Lease:              2 * time.Second,
			SkipCleanup:        false,
			NoColor:            true,
			UseStateEngineLock: true,
		}, slog.New(slog.NewTextHandler(io.Discard, nil)))
		errCh <- err
	}()

	deadline := time.Now().Add(10 * time.Second)
	for {
		var count int
		if err := rig.pool.QueryRow(ctx,
			`SELECT COUNT(*)
			 FROM state_locks sl
			 JOIN states s ON s.id = sl.state_id
			 WHERE s.name = $1 AND sl.path LIKE $2`,
			stateName, "state-engine://%",
		).Scan(&count); err != nil {
			t.Fatalf("query state-engine coarse lock: %v", err)
		}
		if count > 0 {
			break
		}
		select {
		case err := <-errCh:
			if err != nil {
				t.Fatalf("apply.Run exited early: %v", err)
			}
			t.Fatal("apply.Run finished before coarse lock was observed")
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for state-engine coarse lock")
		}
		time.Sleep(50 * time.Millisecond)
	}

	lockInfo := store.LockInfo{
		ID:        "vanilla-lock-attempt",
		Operation: "OperationTypeApply",
		Info:      "integration-test",
		Who:       "vanilla-terraform",
		Version:   "1.13.4",
		Created:   time.Now().UTC().Format(time.RFC3339Nano),
		Path:      stateName,
	}
	current, err := rig.store.AcquireLock(ctx, stateName, lockInfo)
	if !errors.Is(err, store.ErrAlreadyLocked) {
		t.Fatalf("AcquireLock during state-engine apply: want ErrAlreadyLocked, got current=%+v err=%v", current, err)
	}
	if !strings.HasPrefix(current.Path, "state-engine://") {
		t.Fatalf("conflicting lock path=%q want state-engine://...", current.Path)
	}

	if err := <-errCh; err != nil {
		t.Fatalf("apply.Run: %v", err)
	}

	var count int
	if err := rig.pool.QueryRow(ctx,
		`SELECT COUNT(*)
		 FROM state_locks sl
		 JOIN states s ON s.id = sl.state_id
		 WHERE s.name = $1 AND sl.path LIKE $2`,
		stateName, "state-engine://%",
	).Scan(&count); err != nil {
		t.Fatalf("query coarse lock after apply: %v", err)
	}
	if count != 0 {
		t.Fatalf("coarse lock rows after apply=%d want 0", count)
	}
}

func TestApplyOrchestrator_StateEngineCoarseLockReleasedOnCancelAndVanillaLockRecovers(t *testing.T) {
	stateName := "apply-it-state-engine-cancel"
	rig := newApplyRig(t, stateName)
	rig.slowSleepFixture(t, "0s", "30s")

	spec := handCraftSingleAddrPlanSpec(t, rig.workDir, "time_sleep.slow")

	parentCtx, parentCancel := context.WithCancel(testdb.BackgroundTenantCtx())
	defer parentCancel()
	ctx, timeoutCancel := context.WithTimeout(parentCtx, 2*time.Minute)
	defer timeoutCancel()

	type runResult struct {
		res *Result
		err error
	}
	resCh := make(chan runResult, 1)
	go func() {
		res, err := Run(ctx, rig.store, Options{
			Spec:               spec,
			StateName:          stateName,
			Actor:              "integration-test",
			WorkDir:            rig.workDir,
			TerraformBin:       rig.terraformBin,
			Lease:              5 * time.Second,
			SkipCleanup:        false,
			NoColor:            true,
			UseStateEngineLock: true,
		}, slog.New(slog.NewTextHandler(io.Discard, nil)))
		resCh <- runResult{res: res, err: err}
	}()

	deadline := time.Now().Add(15 * time.Second)
	for {
		var count int
		if err := rig.pool.QueryRow(testdb.BackgroundTenantCtx(),
			`SELECT COUNT(*)
			 FROM state_locks sl
			 JOIN states s ON s.id = sl.state_id
			 WHERE s.name = $1 AND sl.path LIKE $2`,
			stateName, "state-engine://%",
		).Scan(&count); err != nil {
			t.Fatalf("query state-engine coarse lock: %v", err)
		}
		if count > 0 {
			break
		}
		select {
		case got := <-resCh:
			if got.err != nil {
				t.Fatalf("apply.Run exited before cancel path was exercised: %v", got.err)
			}
			t.Fatal("apply.Run finished before coarse lock was observed")
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for state-engine coarse lock")
		}
		time.Sleep(50 * time.Millisecond)
	}

	parentCancel()

	got := <-resCh
	if got.err == nil {
		t.Fatalf("expected cancellation error, got nil result=%+v", got.res)
	}
	if !errors.Is(got.err, context.Canceled) && !strings.Contains(strings.ToLower(got.err.Error()), "aborted") {
		t.Fatalf("unexpected cancel error: %v", got.err)
	}
	if got.res == nil || got.res.ApplyID == "" {
		t.Fatalf("expected apply result with ApplyID after cancel, got %+v", got.res)
	}

	var status string
	if err := rig.pool.QueryRow(testdb.BackgroundTenantCtx(),
		`SELECT status FROM apply_runs WHERE id = $1`,
		got.res.ApplyID,
	).Scan(&status); err != nil {
		t.Fatalf("query canceled apply_runs: %v", err)
	}
	if status != "aborted" {
		t.Fatalf("apply_runs.status after cancel = %q want %q", status, "aborted")
	}

	var coarseLocks int
	if err := rig.pool.QueryRow(testdb.BackgroundTenantCtx(),
		`SELECT COUNT(*)
		 FROM state_locks sl
		 JOIN states s ON s.id = sl.state_id
		 WHERE s.name = $1 AND sl.path LIKE $2`,
		stateName, "state-engine://%",
	).Scan(&coarseLocks); err != nil {
		t.Fatalf("query coarse lock after cancel: %v", err)
	}
	if coarseLocks != 0 {
		t.Fatalf("coarse lock rows after cancel=%d want 0", coarseLocks)
	}

	var reservations int
	if err := rig.pool.QueryRow(testdb.BackgroundTenantCtx(),
		`SELECT COUNT(*) FROM resource_reservations WHERE apply_id = $1`,
		got.res.ApplyID,
	).Scan(&reservations); err != nil {
		t.Fatalf("query reservations after cancel: %v", err)
	}
	if reservations != 0 {
		t.Fatalf("reservations after cancel=%d want 0", reservations)
	}

	lockInfo := store.LockInfo{
		ID:        "vanilla-lock-after-cancel",
		Operation: "OperationTypeApply",
		Info:      "integration-test",
		Who:       "vanilla-terraform",
		Version:   "1.13.4",
		Created:   time.Now().UTC().Format(time.RFC3339Nano),
		Path:      stateName,
	}
	current, err := rig.store.AcquireLock(testdb.BackgroundTenantCtx(), stateName, lockInfo)
	if err != nil {
		t.Fatalf("AcquireLock after cancel: current=%+v err=%v", current, err)
	}
	if err := rig.store.ReleaseLock(testdb.BackgroundTenantCtx(), stateName, lockInfo.ID, "vanilla-terraform"); err != nil {
		t.Fatalf("ReleaseLock after cancel: %v", err)
	}
}

func TestApplyOrchestrator_FullTrunkFallbackSpec_StaysOffTrustedStateEngineLane(t *testing.T) {
	stateName := "apply-it-full-trunk-fallback"
	rig := newApplyRig(t, stateName)
	rig.twoResourceFixture(t,
		"old-a", "old-b",
		"new-a", "old-b",
	)
	spec := handCraftPlanSpec(t, rig.workDir, "terraform_data.a")
	spec.StateEngine = &plan.StateEnginePlanMetadata{
		Mode:           "full-trunk-fallback",
		FallbackReason: "native scoped state-engine path unavailable; falling back to full trunk",
	}

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Minute)
	defer cancel()

	res, err := Run(ctx, rig.store, Options{
		Spec:         spec,
		StateName:    stateName,
		Actor:        "integration-test",
		WorkDir:      rig.workDir,
		TerraformBin: rig.terraformBin,
		Lease:        5 * time.Minute,
		SkipCleanup:  false,
		NoColor:      true,
		// This is the critical assertion path: a fallback-classified spec
		// must stay on the legacy snapshot-merge lane and never take the
		// trusted state-engine coarse-lock/delta path.
		UseStateEngineLock: false,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("apply.Run fallback lane: %v\nresult: %+v", err, res)
	}
	if res == nil {
		t.Fatal("apply.Run returned nil result")
	}
	if got, want := res.CommitMode, "snapshot merge"; got != want {
		t.Fatalf("commit mode: got %q want %q", got, want)
	}
	if len(res.NativeIntentWriteSet) != 0 || len(res.NativeIntentDeleteSet) != 0 || strings.TrimSpace(res.NativeIntentSource) != "" {
		t.Fatalf("unexpected native intent fields on fallback lane: source=%q writes=%v deletes=%v",
			res.NativeIntentSource, res.NativeIntentWriteSet, res.NativeIntentDeleteSet)
	}

	var coarseCount int
	if err := rig.pool.QueryRow(ctx,
		`SELECT COUNT(*)
		 FROM state_locks sl
		 JOIN states s ON s.id = sl.state_id
		 WHERE s.name = $1 AND sl.path LIKE $2`,
		stateName, "state-engine://%",
	).Scan(&coarseCount); err != nil {
		t.Fatalf("query state-engine coarse lock rows: %v", err)
	}
	if coarseCount != 0 {
		t.Fatalf("state-engine coarse lock rows=%d want 0 for full-trunk fallback lane", coarseCount)
	}

	raw, err := rig.store.GetCurrentState(ctx, stateName)
	if err != nil {
		t.Fatalf("fetch fallback-lane trunk: %v", err)
	}
	var snap map[string]any
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatalf("parse fallback-lane trunk: %v", err)
	}
	got := extractInputs(t, snap)
	if got["terraform_data.a"] != "new-a" {
		t.Fatalf("post-apply terraform_data.a.input: got %q want %q", got["terraform_data.a"], "new-a")
	}
	if got["terraform_data.b"] != "old-b" {
		t.Fatalf("non-write_set terraform_data.b.input must be preserved: got %q want %q", got["terraform_data.b"], "old-b")
	}
}

func TestApplyOrchestrator_NativeSliceSpec_UsesTrustedStateEngineLane(t *testing.T) {
	stateName := "apply-it-native-slice-trusted"
	rig := newApplyRig(t, stateName)
	rig.twoResourceFixture(t,
		"old-a", "old-b",
		"new-a", "old-b",
	)
	spec := handCraftPlanSpec(t, rig.workDir, "terraform_data.a")
	spec.StateEngine = &plan.StateEnginePlanMetadata{
		Mode:                "native-slice",
		Confidence:          "safe",
		FetchAddresses:      []string{"terraform_data.a"},
		MissingFromState:    []string{},
		UnknownMissing:      []string{},
		ConfigRequiredNodes: nil,
	}

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Minute)
	defer cancel()

	res, err := Run(ctx, rig.store, Options{
		Spec:               spec,
		StateName:          stateName,
		Actor:              "integration-test",
		WorkDir:            rig.workDir,
		TerraformBin:       rig.terraformBin,
		Lease:              5 * time.Minute,
		SkipCleanup:        false,
		NoColor:            true,
		UseStateEngineLock: true,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("apply.Run native trusted lane: %v\nresult: %+v", err, res)
	}
	if res == nil {
		t.Fatal("apply.Run returned nil result")
	}
	if got, want := res.CommitMode, "state-engine delta"; got != want {
		t.Fatalf("commit mode: got %q want %q", got, want)
	}
	if got, want := strings.TrimSpace(res.NativeIntentSource), "terraform validation replan"; got != want {
		t.Fatalf("native intent source: got %q want %q", got, want)
	}
	if got := res.NativeIntentWriteSet; !(len(got) == 1 && got[0] == "terraform_data.a") {
		t.Fatalf("native intent write set: got %v want [terraform_data.a]", got)
	}
	if len(res.NativeIntentDeleteSet) != 0 {
		t.Fatalf("native intent delete set: got %v want empty", res.NativeIntentDeleteSet)
	}
	if got := res.AppliedAddresses; !(len(got) == 1 && got[0] == "terraform_data.a") {
		t.Fatalf("applied addresses: got %v want [terraform_data.a]", got)
	}

	var coarseCount int
	if err := rig.pool.QueryRow(ctx,
		`SELECT COUNT(*)
		 FROM state_locks sl
		 JOIN states s ON s.id = sl.state_id
		 WHERE s.name = $1 AND sl.path LIKE $2`,
		stateName, "state-engine://%",
	).Scan(&coarseCount); err != nil {
		t.Fatalf("query state-engine coarse lock rows after trusted apply: %v", err)
	}
	if coarseCount != 0 {
		t.Fatalf("state-engine coarse lock rows after apply=%d want 0", coarseCount)
	}

	raw, err := rig.store.GetCurrentState(ctx, stateName)
	if err != nil {
		t.Fatalf("fetch trusted-lane trunk: %v", err)
	}
	var snap map[string]any
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatalf("parse trusted-lane trunk: %v", err)
	}
	got := extractInputs(t, snap)
	if got["terraform_data.a"] != "new-a" {
		t.Fatalf("post-apply terraform_data.a.input: got %q want %q", got["terraform_data.a"], "new-a")
	}
	if got["terraform_data.b"] != "old-b" {
		t.Fatalf("non-write_set terraform_data.b.input must be preserved: got %q want %q", got["terraform_data.b"], "old-b")
	}
}

func TestApplyOrchestrator_NativeSliceRemovedConfigDelete_UsesTrustedStateEngineLane(t *testing.T) {
	stateName := "apply-it-native-slice-delete"
	rig := newApplyRig(t, stateName)
	rig.twoResourceFixture(t,
		"old-a", "old-b",
		"old-a", "old-b",
	)

	const deleteHCL = `terraform {
  required_version = ">= 1.4.0"
}

resource "terraform_data" "a" {
  input = "old-a"
}

removed {
  from = terraform_data.b
}
`
	if err := os.WriteFile(filepath.Join(rig.workDir, "main.tf"), []byte(deleteHCL), 0o644); err != nil {
		t.Fatalf("write delete main.tf: %v", err)
	}

	spec := &plan.PlanSpec{
		FormatVersion: plan.CurrentSpecFormatVersion,
		GeneratedAt:   time.Now().UTC(),
		ConfigDir:     rig.workDir,
		PlanSummary: plan.PlanSummary{
			Delete: 1,
			Total:  1,
		},
		WriteSet:     []string{"terraform_data.b"},
		ReadSet:      []string{"terraform_data.b"},
		HCLFootprint: []string{"terraform_data.a", "terraform_data.b"},
		Reservations: []plan.PlanReservation{
			{Address: "terraform_data.b", Mode: "write"},
		},
		StateEngine: &plan.StateEnginePlanMetadata{
			Mode:                "native-slice",
			Confidence:          "safe",
			FetchAddresses:      []string{"terraform_data.a", "terraform_data.b"},
			RemovedConfigNodes:  []string{"terraform_data.b"},
			MissingFromState:    []string{},
			UnknownMissing:      []string{},
			ConfigRequiredNodes: nil,
		},
	}

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Minute)
	defer cancel()

	res, err := Run(ctx, rig.store, Options{
		Spec:               spec,
		StateName:          stateName,
		Actor:              "integration-test",
		WorkDir:            rig.workDir,
		TerraformBin:       rig.terraformBin,
		Lease:              5 * time.Minute,
		SkipCleanup:        false,
		NoColor:            true,
		UseStateEngineLock: true,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("apply.Run native trusted delete lane: %v\nresult: %+v", err, res)
	}
	if res == nil {
		t.Fatal("apply.Run returned nil result")
	}
	if got, want := res.CommitMode, "state-engine delta"; got != want {
		t.Fatalf("commit mode: got %q want %q", got, want)
	}
	if got, want := strings.TrimSpace(res.NativeIntentSource), "terraform validation replan"; got != want {
		t.Fatalf("native intent source: got %q want %q", got, want)
	}
	if got := res.NativeIntentWriteSet; !(len(got) == 1 && got[0] == "terraform_data.b") {
		t.Fatalf("native intent write set: got %v want [terraform_data.b]", got)
	}
	if got := res.NativeIntentDeleteSet; !(len(got) == 1 && got[0] == "terraform_data.b") {
		t.Fatalf("native intent delete set: got %v want [terraform_data.b]", got)
	}
	if got := res.AppliedAddresses; !(len(got) == 1 && got[0] == "terraform_data.b") {
		t.Fatalf("applied addresses: got %v want [terraform_data.b]", got)
	}

	raw, err := rig.store.GetCurrentState(ctx, stateName)
	if err != nil {
		t.Fatalf("fetch trusted-delete trunk: %v", err)
	}
	var snap map[string]any
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatalf("parse trusted-delete trunk: %v", err)
	}
	got := extractInputs(t, snap)
	if got["terraform_data.a"] != "old-a" {
		t.Fatalf("post-apply terraform_data.a.input: got %q want %q", got["terraform_data.a"], "old-a")
	}
	if _, ok := got["terraform_data.b"]; ok {
		t.Fatalf("terraform_data.b should have been deleted, but is still present: %v", got)
	}
}

func TestApplyOrchestrator_StalePlanRejectedWhenReadSetChanged(t *testing.T) {
	stateName := "apply-it-stale"
	rig := newApplyRig(t, stateName)
	rig.twoResourceFixture(t,
		"old-a", "old-b",
		"new-a", "old-b",
	)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Minute)
	defer cancel()

	// Capture the serial we planned against.
	info, err := rig.store.GetCurrentStateInfo(ctx, stateName)
	if err != nil {
		t.Fatalf("get trunk: %v", err)
	}
	src := info.Serial

	// Simulate another writer changing terraform_data.b after the plan.
	otherDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(otherDir, "main.tf"), []byte(fmt.Sprintf(`terraform {
  required_version = ">= 1.4.0"
}

resource "terraform_data" "a" {
  input = "%s"
}

resource "terraform_data" "b" {
  input = "%s"
}
`, "old-a", "new-b")), 0o644); err != nil {
		t.Fatalf("write other main.tf: %v", err)
	}
	for _, args := range [][]string{
		{"init", "-input=false", "-no-color"},
		{"apply", "-auto-approve", "-input=false", "-no-color"},
	} {
		cmd := exec.CommandContext(ctx, rig.terraformBin, args...)
		cmd.Dir = otherDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("other terraform %v: %v\n%s", args, err, out)
		}
	}
	otherStateBytes, err := os.ReadFile(filepath.Join(otherDir, "terraform.tfstate"))
	if err != nil {
		t.Fatalf("read other state: %v", err)
	}
	if err := rig.store.WriteState(ctx, stateName, "", otherStateBytes, "import", "other-writer"); err != nil {
		t.Fatalf("write other trunk: %v", err)
	}

	// Now try to apply using a spec pinned to the old serial with a read-set
	// that includes terraform_data.b. The staleness guard should reject.
	spec := handCraftPlanSpec(t, rig.workDir, "terraform_data.a")
	spec.ReadSet = []string{"terraform_data.a", "terraform_data.b"}
	spec.Reservations = []plan.PlanReservation{
		{Address: "terraform_data.a", Mode: "write"},
		{Address: "terraform_data.b", Mode: "read"},
	}
	spec.SourceSerial = &src

	_, err = Run(ctx, rig.store, Options{
		Spec:         spec,
		StateName:    stateName,
		Actor:        "integration-test",
		WorkDir:      rig.workDir,
		TerraformBin: rig.terraformBin,
		Lease:        5 * time.Minute,
		SkipCleanup:  false,
		NoColor:      true,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		t.Fatal("expected stale plan rejection, got nil")
	}
	if !strings.Contains(err.Error(), "plan is stale") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestApplyOrchestrator_ReservationConflictAbortsBeforeTerraform
// proves the orchestrator's first defense kicks in: if a second
// apply tries to write the same address while the first holds its
// reservation, the second fails fast from AcquireReservations
// without setting up the tmp dir, running terraform, or modifying
// trunk.
//
// We simulate "the first apply is still running" by inserting a
// non-expiring reservation by hand instead of actually parallelizing
// two terraform processes — the conflict matrix is the property
// under test here, the threading model is covered by v2a's tests.
func TestApplyOrchestrator_ReservationConflictAbortsBeforeTerraform(t *testing.T) {
	stateName := "apply-it-conflict"
	rig := newApplyRig(t, stateName)
	rig.twoResourceFixture(t, "old-a", "old-b", "new-a", "old-b")

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 2*time.Minute)
	defer cancel()

	// Begin a phantom "first apply" that holds a write reservation
	// on terraform_data.a indefinitely.
	trunk, err := rig.store.GetCurrentStateInfo(ctx, stateName)
	if err != nil {
		t.Fatalf("get trunk: %v", err)
	}
	phantom, err := rig.store.BeginApplyRun(ctx,
		trunk.StateID, trunk.VersionID, "phantom",
		trunk.Serial, nil,
	)
	if err != nil {
		t.Fatalf("phantom BeginApplyRun: %v", err)
	}
	if err := rig.store.AcquireReservations(ctx,
		trunk.StateID, phantom.ID, "phantom",
		[]store.Reservation{{AddressGlob: "terraform_data.a", Mode: store.ReservationWrite}},
		1*time.Hour,
	); err != nil {
		t.Fatalf("phantom acquire: %v", err)
	}

	// Our apply tries to acquire write on the same address.
	spec := handCraftPlanSpec(t, rig.workDir, "terraform_data.a")
	res, err := Run(ctx, rig.store, Options{
		Spec:         spec,
		StateName:    stateName,
		Actor:        "loser",
		WorkDir:      rig.workDir,
		TerraformBin: rig.terraformBin,
		Lease:        5 * time.Minute,
		NoColor:      true,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		t.Fatalf("expected reservation conflict error; got nil. result: %+v", res)
	}

	// Trunk must not have advanced.
	freshInfo, err := rig.store.GetCurrentStateInfo(ctx, stateName)
	if err != nil {
		t.Fatalf("re-read trunk: %v", err)
	}
	if freshInfo.Serial != trunk.Serial {
		t.Errorf("trunk serial advanced despite conflict: was %d, now %d",
			trunk.Serial, freshInfo.Serial)
	}

	// The audit row for the loser should be 'failed'.
	if res == nil || res.ApplyID == "" {
		t.Fatalf("expected non-nil result with ApplyID, got %+v", res)
	}
	var status string
	if err := rig.pool.QueryRow(ctx,
		`SELECT status FROM apply_runs WHERE id = $1`,
		res.ApplyID,
	).Scan(&status); err != nil {
		t.Fatalf("query loser apply_runs: %v", err)
	}
	if status != "failed" {
		t.Errorf("loser apply_runs.status: got %q want %q", status, "failed")
	}
}

// extractInputs returns address -> attributes.input for every
// resource in the parsed state. The state JSON shape is the
// terraform v4 layout the rest of the codebase expects.
func extractInputs(t *testing.T, snap map[string]any) map[string]string {
	t.Helper()
	out := map[string]string{}
	resources, _ := snap["resources"].([]any)
	for _, ri := range resources {
		r, _ := ri.(map[string]any)
		ty, _ := r["type"].(string)
		name, _ := r["name"].(string)
		mode, _ := r["mode"].(string)
		addr := ty + "." + name
		if mode == "data" {
			addr = "data." + addr
		}
		if mod, ok := r["module"].(string); ok && mod != "" {
			addr = mod + "." + addr
		}
		instances, _ := r["instances"].([]any)
		if len(instances) == 0 {
			continue
		}
		inst0, _ := instances[0].(map[string]any)
		attrs, _ := inst0["attributes"].(map[string]any)
		// terraform_data.input is a dynamic-typed field that lands
		// in state as {"value": ..., "type": "string"}. Unwrap to
		// the plain string before comparison.
		if input, ok := attrs["input"].(map[string]any); ok {
			if v, ok := input["value"].(string); ok {
				out[addr] = v
			}
		} else if v, ok := attrs["input"].(string); ok {
			out[addr] = v
		}
	}
	return out
}
