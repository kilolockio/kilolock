//go:build integration

package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/davesade/kilolock/internal/testdb"
)

func testStateLifecycleRaw(t *testing.T, serial int64, lineage string) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"version":           4,
		"terraform_version": "1.13.4",
		"serial":            serial,
		"lineage":           lineage,
		"outputs":           map[string]any{},
		"resources":         []any{},
	})
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	return raw
}

func TestStateLifecycle_ArchiveHidesStateAndBlocksWritesUntilRestore(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	mustResetTables(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 10*time.Second)
	defer cancel()

	const stateName = "archive-test"
	if err := s.WriteState(ctx, stateName, "", testStateLifecycleRaw(t, 1, "11111111-2222-3333-4444-555555555555"), "itest", "itest"); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	if err := s.SetStateLifecycleStatusAudit(ctx, stateName, LifecycleStatusArchived, "itest", "archive for support flow"); err != nil {
		t.Fatalf("archive state: %v", err)
	}
	if _, err := s.GetCurrentState(ctx, stateName); !errors.Is(err, ErrStateNotFound) {
		t.Fatalf("GetCurrentState after archive err=%v want ErrStateNotFound", err)
	}
	err := s.WriteState(ctx, stateName, "", testStateLifecycleRaw(t, 2, "11111111-2222-3333-4444-555555555555"), "itest", "itest")
	var inactive *StateNotActiveError
	if !errors.As(err, &inactive) {
		t.Fatalf("WriteState after archive err=%v want StateNotActiveError", err)
	}
	if inactive.Status != LifecycleStatusArchived {
		t.Fatalf("inactive status=%q want %q", inactive.Status, LifecycleStatusArchived)
	}
	if err := s.SetStateLifecycleStatusAudit(ctx, stateName, LifecycleStatusActive, "itest", "restore for support flow"); err != nil {
		t.Fatalf("restore state: %v", err)
	}
	if _, err := s.GetCurrentState(ctx, stateName); err != nil {
		t.Fatalf("GetCurrentState after restore: %v", err)
	}
}

func TestStateLifecycle_ArchiveRenamesStateAndFreesOriginalForReuse(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	mustResetTables(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 10*time.Second)
	defer cancel()

	const stateName = "reuse-me"
	if err := s.WriteState(ctx, stateName, "", testStateLifecycleRaw(t, 1, "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"), "itest", "itest"); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	if err := s.SetStateLifecycleStatusAudit(ctx, stateName, LifecycleStatusArchived, "itest", "cleanup"); err != nil {
		t.Fatalf("archive state: %v", err)
	}
	if _, err := s.GetCurrentState(ctx, stateName); !errors.Is(err, ErrStateNotFound) {
		t.Fatalf("GetCurrentState after archive err=%v want ErrStateNotFound", err)
	}
	if err := s.WriteState(ctx, stateName, "", testStateLifecycleRaw(t, 2, "ffffffff-1111-2222-3333-444444444444"), "itest", "itest"); err != nil {
		t.Fatalf("recreate state with original name: %v", err)
	}
	if _, err := s.GetCurrentState(ctx, stateName); err != nil {
		t.Fatalf("GetCurrentState recreated state: %v", err)
	}
	var archivedName string
	if err := s.pool.QueryRow(ctx, `SELECT name FROM states WHERE lifecycle_status = 'archived' LIMIT 1`).Scan(&archivedName); err != nil {
		t.Fatalf("query archived state name: %v", err)
	}
	if archivedName == stateName || !strings.HasPrefix(archivedName, stateName+"--archived-") {
		t.Fatalf("archived state name=%q want prefix %q", archivedName, stateName+"--archived-")
	}
}
