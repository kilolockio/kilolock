//go:build integration

// Run with:
//
//	KL_DATABASE_URL=postgres://kl:kl@localhost:5432/kl?sslmode=disable \
//	  go test -tags=integration -run 'TestTags' ./pkg/store/...

package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/davesade/kilolock/internal/testdb"
)

// TestTags_SetMoveListUnset is the lifecycle smoke. Creates a tag,
// moves it to another version (the move is the part operators
// care about: "I want pre-mig to mean LATEST stable now"),
// resolves the tag through GetVersionRaw, lists, unsets, and
// verifies an UnsetTag on the now-missing tag returns
// ErrTagNotFound.
func TestTags_SetMoveListUnset(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	mustResetTables(t, pool)

	// Two versions: v1 (serial 1) and v2 (serial 2; becomes current).
	seedStateWithResources(t, s, "qtest", 1, map[string]string{
		"aws_vpc.main": "vpc-1",
	})
	seedStateWithResources(t, s, "qtest", 2, map[string]string{
		"aws_vpc.main": "vpc-2",
	})

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 10*time.Second)
	defer cancel()

	v1 := mustResolve(t, s, "qtest", "1")
	v2 := mustResolve(t, s, "qtest", "2")

	// Set: pre-mig → v1.
	row, err := s.SetTag(ctx, "qtest", "1", "pre-mig", "before db migration", "alice")
	if err != nil {
		t.Fatalf("SetTag pre-mig→1: %v", err)
	}
	if row.VersionID != v1.ID || row.Serial != 1 {
		t.Errorf("set row points at %s (serial %d), want %s (serial 1)", row.VersionID, row.Serial, v1.ID)
	}
	if row.Description != "before db migration" {
		t.Errorf("description not persisted: %q", row.Description)
	}

	// Move: pre-mig → v2 (current).
	row, err = s.SetTag(ctx, "qtest", "current", "pre-mig", "updated", "bob")
	if err != nil {
		t.Fatalf("SetTag move pre-mig→current: %v", err)
	}
	if row.VersionID != v2.ID {
		t.Errorf("move row points at %s, want %s", row.VersionID, v2.ID)
	}
	if row.Actor != "bob" {
		t.Errorf("actor not updated on move: %q", row.Actor)
	}

	// Resolve via GetVersionRaw: passing tag name as ref must
	// land on v2.
	resolved, _, err := s.GetVersionRaw(ctx, "qtest", "pre-mig")
	if err != nil {
		t.Fatalf("GetVersionRaw('pre-mig'): %v", err)
	}
	if resolved.ID != v2.ID {
		t.Errorf("tag resolved to %s, want %s", resolved.ID, v2.ID)
	}

	// List: exactly one tag.
	tags, err := s.ListTags(ctx, "qtest")
	if err != nil {
		t.Fatalf("ListTags: %v", err)
	}
	if len(tags) != 1 || tags[0].Tag != "pre-mig" {
		t.Errorf("ListTags = %+v, want one row tag=pre-mig", tags)
	}
	if tags[0].Serial != 2 {
		t.Errorf("listed serial = %d, want 2 (current after move)", tags[0].Serial)
	}

	// Unset.
	if err := s.UnsetTag(ctx, "qtest", "pre-mig", "alice"); err != nil {
		t.Fatalf("UnsetTag: %v", err)
	}

	// Second unset returns ErrTagNotFound.
	if err := s.UnsetTag(ctx, "qtest", "pre-mig", "alice"); !errors.Is(err, ErrTagNotFound) {
		t.Errorf("second UnsetTag err = %v, want ErrTagNotFound", err)
	}
}

// TestTags_ReservedNameRejected: the SQL CHECK constraint must
// reject reserved names so that even a path through the store
// that bypasses ValidateTagName cannot land an invalid row.
// We trigger this by calling SetTag with a forbidden name; the
// expectation is the call fails (either at Go-validation or the
// CHECK level).
func TestTags_ReservedNameRejected(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	mustResetTables(t, pool)

	seedStateWithResources(t, s, "qtest", 1, map[string]string{
		"aws_vpc.main": "vpc-1",
	})

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Second)
	defer cancel()

	for _, bad := range []string{"current", "@0", "42"} {
		_, err := s.SetTag(ctx, "qtest", "current", bad, "", "alice")
		if err == nil {
			t.Errorf("SetTag(%q) succeeded, want reserved-name rejection", bad)
		}
	}
}

// TestTags_ListTagsByVersionID is the batched lookup the CLI
// uses for history annotation. Two tags pointing at the same
// version, one at a different version: must come back grouped
// in a (version_id → tags) map without N+1.
func TestTags_ListTagsByVersionID(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	mustResetTables(t, pool)

	seedStateWithResources(t, s, "qtest", 1, map[string]string{
		"aws_vpc.main": "vpc-1",
	})
	seedStateWithResources(t, s, "qtest", 2, map[string]string{
		"aws_vpc.main": "vpc-2",
	})

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Second)
	defer cancel()

	v1 := mustResolve(t, s, "qtest", "1")
	v2 := mustResolve(t, s, "qtest", "2")

	if _, err := s.SetTag(ctx, "qtest", "1", "pre-mig", "", "alice"); err != nil {
		t.Fatalf("set pre-mig: %v", err)
	}
	if _, err := s.SetTag(ctx, "qtest", "1", "rollback-target", "", "alice"); err != nil {
		t.Fatalf("set rollback-target: %v", err)
	}
	if _, err := s.SetTag(ctx, "qtest", "2", "prod", "", "alice"); err != nil {
		t.Fatalf("set prod: %v", err)
	}

	got, err := s.ListTagsByVersionID(ctx, []string{v1.ID, v2.ID})
	if err != nil {
		t.Fatalf("ListTagsByVersionID: %v", err)
	}
	if len(got[v1.ID]) != 2 {
		t.Errorf("v1 tags = %v, want 2 entries", got[v1.ID])
	}
	if len(got[v2.ID]) != 1 || got[v2.ID][0] != "prod" {
		t.Errorf("v2 tags = %v, want [prod]", got[v2.ID])
	}
}
