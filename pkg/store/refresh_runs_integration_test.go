//go:build integration

// Integration tests for the refresh_runs store layer (v1.6a).
// Run with:
//
//	KL_DATABASE_URL=postgres://kl:kl@localhost:5432/kl?sslmode=disable \
//	  go test -tags=integration ./pkg/store/...

package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/davesade/kilolock/internal/db"
	"github.com/davesade/kilolock/internal/testdb"
)

// resetRefreshRuns truncates the refresh_runs table between tests
// without touching the underlying state. Tests that also need a
// fresh state should call mustResetTables first; this helper is
// composable on top.
func resetRefreshRuns(t *testing.T, pool *db.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `TRUNCATE TABLE refresh_runs`); err != nil {
		t.Fatalf("truncate refresh_runs: %v", err)
	}
}

// seedStateAndVersion inserts a minimal state + state_version pair
// and returns their ids. refresh_runs has FKs into both, so every
// test in this file needs a real ancestor row before it can Begin
// anything.
func seedStateAndVersion(t *testing.T, s *Store) (stateID, versionID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 10*time.Second)
	defer cancel()

	// Reuse the same minimal state_version body the query tests use:
	// one resource, valid Terraform v4 state, fixed lineage. WriteState
	// performs the canonical insert (state, state_version, normalized
	// resources) so we don't have to special-case anything.
	body := []byte(`{
		"version": 4,
		"terraform_version": "1.13.4",
		"serial": 1,
		"lineage": "9b39e2c0-aaaa-bbbb-cccc-444455556666",
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
	const name = "refresh-runs-test"
	if err := s.WriteState(ctx, name, "", body, "test", "test"); err != nil {
		t.Fatalf("WriteState: %v", err)
	}

	err := s.pool.QueryRow(ctx,
		`SELECT s.id, s.current_version_id FROM states s WHERE s.name = $1`,
		name,
	).Scan(&stateID, &versionID)
	if err != nil {
		t.Fatalf("lookup state: %v", err)
	}
	return stateID, versionID
}

func TestRefreshRun_BeginInsertsRunningRow(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	mustResetTables(t, pool)
	resetRefreshRuns(t, pool)
	stateID, fromID := seedStateAndVersion(t, s)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Second)
	defer cancel()

	run, err := s.BeginRefreshRun(ctx, stateID, fromID, "alice")
	if err != nil {
		t.Fatalf("BeginRefreshRun: %v", err)
	}
	if run.ID == "" {
		t.Fatal("ID should be populated by RETURNING")
	}
	if run.StateID != stateID {
		t.Errorf("StateID: got %q, want %q", run.StateID, stateID)
	}
	if run.FromVersionID != fromID {
		t.Errorf("FromVersionID: got %q, want %q", run.FromVersionID, fromID)
	}
	if run.Status != RefreshRunRunning {
		t.Errorf("Status: got %q, want %q", run.Status, RefreshRunRunning)
	}
	if run.FinishedAt != nil {
		t.Errorf("FinishedAt: got %v, want nil", *run.FinishedAt)
	}
	if run.ToVersionID != nil {
		t.Errorf("ToVersionID: got %v, want nil", *run.ToVersionID)
	}
	if run.ResourcesChecked != nil || run.ResourcesChanged != nil || run.ResourcesFailed != nil {
		t.Errorf("counter fields should be nil pre-Finish, got %+v %+v %+v",
			run.ResourcesChecked, run.ResourcesChanged, run.ResourcesFailed)
	}
	if run.Actor == nil || *run.Actor != "alice" {
		t.Errorf("Actor: got %v, want alice", run.Actor)
	}
	if time.Since(run.StartedAt) > 5*time.Second {
		t.Errorf("StartedAt looks stale: %v", run.StartedAt)
	}
}

func TestRefreshRun_BeginEmptyActorIsNULL(t *testing.T) {
	// Empty actor strings should land as NULL on disk so subsequent
	// Get rounds-trip them as a nil pointer rather than an empty
	// string. Subtle but important for the CLI's "(unknown actor)"
	// fallback display.
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	mustResetTables(t, pool)
	resetRefreshRuns(t, pool)
	stateID, fromID := seedStateAndVersion(t, s)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Second)
	defer cancel()

	run, err := s.BeginRefreshRun(ctx, stateID, fromID, "")
	if err != nil {
		t.Fatalf("BeginRefreshRun: %v", err)
	}
	got, err := s.GetRefreshRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRefreshRun: %v", err)
	}
	if got.Actor != nil {
		t.Errorf("Actor: got %v, want nil", *got.Actor)
	}
}

func TestRefreshRun_RejectsEmptyIDs(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	mustResetTables(t, pool)
	resetRefreshRuns(t, pool)
	stateID, fromID := seedStateAndVersion(t, s)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Second)
	defer cancel()

	if _, err := s.BeginRefreshRun(ctx, "", fromID, ""); err == nil {
		t.Error("expected error for empty stateID")
	}
	if _, err := s.BeginRefreshRun(ctx, stateID, "", ""); err == nil {
		t.Error("expected error for empty fromVersionID")
	}
}

func TestRefreshRun_FinishSucceeded(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	mustResetTables(t, pool)
	resetRefreshRuns(t, pool)
	stateID, fromID := seedStateAndVersion(t, s)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 10*time.Second)
	defer cancel()

	run, err := s.BeginRefreshRun(ctx, stateID, fromID, "")
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	// For a real refresh the orchestrator would WriteState a fresh
	// version and pass that ID; for the audit-row test we stand in
	// with the same from_version_id since the column FK doesn't
	// care which version it points at.
	err = s.FinishRefreshRun(ctx, run.ID, FinishRefreshRunInput{
		Status:           RefreshRunSucceeded,
		ToVersionID:      fromID,
		ResourcesChecked: 7,
		ResourcesChanged: 2,
		ResourcesFailed:  0,
	})
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}

	got, err := s.GetRefreshRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != RefreshRunSucceeded {
		t.Errorf("Status: got %q, want %q", got.Status, RefreshRunSucceeded)
	}
	if got.FinishedAt == nil {
		t.Fatal("FinishedAt should be populated post-finish")
	}
	if got.FinishedAt.Before(got.StartedAt) {
		t.Errorf("FinishedAt %v before StartedAt %v", got.FinishedAt, got.StartedAt)
	}
	if got.ToVersionID == nil || *got.ToVersionID != fromID {
		t.Errorf("ToVersionID: got %v, want %q", got.ToVersionID, fromID)
	}
	if got.ResourcesChecked == nil || *got.ResourcesChecked != 7 {
		t.Errorf("ResourcesChecked: got %v, want 7", got.ResourcesChecked)
	}
	if got.ResourcesChanged == nil || *got.ResourcesChanged != 2 {
		t.Errorf("ResourcesChanged: got %v, want 2", got.ResourcesChanged)
	}
	if got.ResourcesFailed == nil || *got.ResourcesFailed != 0 {
		t.Errorf("ResourcesFailed: got %v, want 0", got.ResourcesFailed)
	}
	if got.ErrorSummary != nil {
		t.Errorf("ErrorSummary on success: got %v, want nil", *got.ErrorSummary)
	}
}

func TestRefreshRun_FinishFailedCarriesSummary(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	mustResetTables(t, pool)
	resetRefreshRuns(t, pool)
	stateID, fromID := seedStateAndVersion(t, s)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 10*time.Second)
	defer cancel()

	run, err := s.BeginRefreshRun(ctx, stateID, fromID, "")
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	err = s.FinishRefreshRun(ctx, run.ID, FinishRefreshRunInput{
		Status:           RefreshRunFailed,
		ResourcesChecked: 3,
		ResourcesChanged: 0,
		ResourcesFailed:  1,
		ErrorSummary:     "provider hashicorp/aws crashed during ReadResource",
	})
	if err != nil {
		t.Fatalf("Finish failed: %v", err)
	}

	got, err := s.GetRefreshRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != RefreshRunFailed {
		t.Errorf("Status: got %q, want %q", got.Status, RefreshRunFailed)
	}
	if got.ToVersionID != nil {
		t.Errorf("ToVersionID on failure: got %v, want nil", *got.ToVersionID)
	}
	if got.ErrorSummary == nil ||
		*got.ErrorSummary != "provider hashicorp/aws crashed during ReadResource" {
		t.Errorf("ErrorSummary: got %v", got.ErrorSummary)
	}
}

func TestRefreshRun_FinishRejectsNonTerminalStatus(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	mustResetTables(t, pool)
	resetRefreshRuns(t, pool)
	stateID, fromID := seedStateAndVersion(t, s)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Second)
	defer cancel()

	run, err := s.BeginRefreshRun(ctx, stateID, fromID, "")
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	err = s.FinishRefreshRun(ctx, run.ID, FinishRefreshRunInput{
		Status: RefreshRunRunning,
	})
	if err == nil {
		t.Fatal("expected error finishing with status='running', got nil")
	}
}

func TestRefreshRun_FinishRejectsNegativeCounters(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	mustResetTables(t, pool)
	resetRefreshRuns(t, pool)
	stateID, fromID := seedStateAndVersion(t, s)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Second)
	defer cancel()

	run, err := s.BeginRefreshRun(ctx, stateID, fromID, "")
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	err = s.FinishRefreshRun(ctx, run.ID, FinishRefreshRunInput{
		Status:           RefreshRunSucceeded,
		ResourcesChecked: -1,
	})
	if err == nil {
		t.Fatal("expected error for negative counter, got nil")
	}
}

func TestRefreshRun_DoubleFinishIsRejected(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	mustResetTables(t, pool)
	resetRefreshRuns(t, pool)
	stateID, fromID := seedStateAndVersion(t, s)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 10*time.Second)
	defer cancel()

	run, err := s.BeginRefreshRun(ctx, stateID, fromID, "")
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	if err := s.FinishRefreshRun(ctx, run.ID, FinishRefreshRunInput{
		Status:      RefreshRunSucceeded,
		ToVersionID: fromID,
	}); err != nil {
		t.Fatalf("first Finish: %v", err)
	}

	err = s.FinishRefreshRun(ctx, run.ID, FinishRefreshRunInput{
		Status: RefreshRunCancelled,
	})
	if !errors.Is(err, ErrRefreshRunAlreadyFinished) {
		t.Fatalf("second Finish: got %v, want ErrRefreshRunAlreadyFinished", err)
	}
}

func TestRefreshRun_FinishMissingIDReturnsSentinel(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	mustResetTables(t, pool)
	resetRefreshRuns(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Second)
	defer cancel()

	err := s.FinishRefreshRun(ctx, "00000000-0000-0000-0000-000000000000",
		FinishRefreshRunInput{Status: RefreshRunSucceeded})
	if !errors.Is(err, ErrRefreshRunNotFound) {
		t.Fatalf("got %v, want ErrRefreshRunNotFound", err)
	}
}

func TestRefreshRun_GetMissingReturnsSentinel(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	mustResetTables(t, pool)
	resetRefreshRuns(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Second)
	defer cancel()

	_, err := s.GetRefreshRun(ctx, "00000000-0000-0000-0000-000000000000")
	if !errors.Is(err, ErrRefreshRunNotFound) {
		t.Fatalf("got %v, want ErrRefreshRunNotFound", err)
	}
}

func TestRefreshRun_ListReturnsNewestFirst(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	mustResetTables(t, pool)
	resetRefreshRuns(t, pool)
	stateID, fromID := seedStateAndVersion(t, s)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 10*time.Second)
	defer cancel()

	first, err := s.BeginRefreshRun(ctx, stateID, fromID, "")
	if err != nil {
		t.Fatalf("first Begin: %v", err)
	}
	// Postgres clock resolution is sub-microsecond; a brief sleep
	// makes the second started_at observably later.
	time.Sleep(15 * time.Millisecond)
	second, err := s.BeginRefreshRun(ctx, stateID, fromID, "")
	if err != nil {
		t.Fatalf("second Begin: %v", err)
	}

	runs, err := s.ListRefreshRuns(ctx, "refresh-runs-test", 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("List: got %d runs, want 2", len(runs))
	}
	if runs[0].ID != second.ID {
		t.Errorf("List[0]: got %q, want %q (newest first)", runs[0].ID, second.ID)
	}
	if runs[1].ID != first.ID {
		t.Errorf("List[1]: got %q, want %q", runs[1].ID, first.ID)
	}
}

func TestRefreshRun_ListRespectsLimit(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	mustResetTables(t, pool)
	resetRefreshRuns(t, pool)
	stateID, fromID := seedStateAndVersion(t, s)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 10*time.Second)
	defer cancel()

	for i := 0; i < 3; i++ {
		if _, err := s.BeginRefreshRun(ctx, stateID, fromID, ""); err != nil {
			t.Fatalf("Begin #%d: %v", i, err)
		}
		time.Sleep(2 * time.Millisecond)
	}

	runs, err := s.ListRefreshRuns(ctx, "refresh-runs-test", 2)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(runs) != 2 {
		t.Errorf("List with limit=2: got %d, want 2", len(runs))
	}
}

func TestRefreshRun_ListUnknownStateReturnsEmpty(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	mustResetTables(t, pool)
	resetRefreshRuns(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Second)
	defer cancel()

	runs, err := s.ListRefreshRuns(ctx, "no-such-state", 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(runs) != 0 {
		t.Errorf("List: got %d, want 0", len(runs))
	}
}

func TestRefreshRun_StatusIsTerminal(t *testing.T) {
	// IsTerminal is the only piece of behavior in the package that
	// can be exercised without a database; pin it down so future
	// reorderings of the constant block don't silently break the
	// classification.
	cases := map[RefreshRunStatus]bool{
		RefreshRunRunning:    false,
		RefreshRunSucceeded:  true,
		RefreshRunFailed:     true,
		RefreshRunCancelled:  true,
		RefreshRunStatus(""): false,
	}
	for st, want := range cases {
		if got := st.IsTerminal(); got != want {
			t.Errorf("(%q).IsTerminal() = %v, want %v", st, got, want)
		}
	}
}
