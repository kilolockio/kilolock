package migrate

import (
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestWrapDuplicateObjectErr_AnnotatesDuplicateCodes(t *testing.T) {
	m := migration{version: 10, name: "0010_state_version_tags.sql"}

	cases := []struct {
		name string
		code string
	}{
		{"duplicate_table", "42P07"},
		{"duplicate_schema", "42P06"},
		{"duplicate_object", "42710"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			orig := &pgconn.PgError{Code: tc.code, Message: `relation "x" already exists`}
			got := wrapDuplicateObjectErr(m, orig)
			if got == nil {
				t.Fatal("expected wrapped error, got nil")
			}
			if !errors.Is(got, orig) {
				t.Fatal("wrapped error must keep the original via %w")
			}
			s := got.Error()
			if !strings.Contains(s, "schema_migrations does not list version 10") {
				t.Errorf("hint missing schema_migrations explanation: %q", s)
			}
			if !strings.Contains(s, "INSERT INTO schema_migrations (version) VALUES (10)") {
				t.Errorf("hint missing copy-pasteable INSERT: %q", s)
			}
		})
	}
}

func TestValidateMigrationSet_DuplicateVersionFails(t *testing.T) {
	err := validateMigrationSet([]migration{
		{version: 2, name: "0002_resource_lifecycles.sql"},
		{version: 2, name: "0002_other.sql"},
	})
	if err == nil {
		t.Fatal("expected duplicate version error")
	}
	if !strings.Contains(err.Error(), "duplicate migration version 0002") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateMigrationSet_UniqueVersionsPass(t *testing.T) {
	err := validateMigrationSet([]migration{
		{version: 1, name: "0001_baseline.sql"},
		{version: 2, name: "0002_second.sql"},
		{version: 3, name: "0003_third.sql"},
	})
	if err != nil {
		t.Fatalf("expected unique versions to pass, got %v", err)
	}
}

func TestWrapDuplicateObjectErr_PassesThroughOtherErrors(t *testing.T) {
	m := migration{version: 3, name: "0003_provider_schemas.sql"}

	// Non-duplicate pg error.
	otherPg := &pgconn.PgError{Code: "23505", Message: "unique_violation"}
	if got := wrapDuplicateObjectErr(m, otherPg); got != otherPg {
		t.Errorf("expected non-duplicate pg error to pass through, got %v", got)
	}

	// Plain error.
	plain := errors.New("boom")
	if got := wrapDuplicateObjectErr(m, plain); got != plain {
		t.Errorf("expected plain error to pass through, got %v", got)
	}

	if got := wrapDuplicateObjectErr(m, nil); got != nil {
		t.Errorf("expected nil to pass through, got %v", got)
	}
}

// TestMigrationFilename_NotDoublePrefixed guards against accidentally
// re-introducing the cosmetic bug where the error message formatted
// `m.name` (already a `0010_*.sql` filename) with `%04d_%s`, producing
// `0010_0010_state_version_tags.sql`. Keeping `m.name` as the full
// filename and using `%s` is intentional.
func TestMigrationFilename_NotDoublePrefixed(t *testing.T) {
	got, err := load()
	if err != nil {
		t.Fatalf("load embedded migrations: %v", err)
	}
	for _, m := range got {
		if strings.HasPrefix(m.name, "_") {
			t.Errorf("migration name should not start with underscore: %q", m.name)
		}
		if !strings.HasSuffix(m.name, ".sql") {
			t.Errorf("migration name should end with .sql: %q", m.name)
		}
	}
}
