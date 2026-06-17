//go:build integration

// Run with:
//
//	KL_DATABASE_URL=postgres://kl:kl@localhost:5432/kl?sslmode=disable \
//	  go test -tags=integration ./pkg/store/...

package store

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/kilolockio/kilolock/pkg/auth"
	"github.com/kilolockio/kilolock/pkg/db"
	"github.com/kilolockio/kilolock/pkg/migrate"
	"github.com/kilolockio/kilolock/pkg/testdb"
)

func openTestStore(t *testing.T) (*Store, *db.Pool) {
	t.Helper()
	url := os.Getenv("KL_DATABASE_URL")
	if url == "" {
		url = os.Getenv("DATABASE_URL")
	}
	if url == "" {
		t.Skip("no KL_DATABASE_URL or DATABASE_URL set")
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
	return New(pool.Pool), pool
}

func seedSimpleState(t *testing.T, s *Store) {
	t.Helper()
	body := map[string]any{
		"version":           4,
		"terraform_version": "1.13.4",
		"serial":            1,
		"lineage":           "9b39e2c0-aaaa-bbbb-cccc-444455556666",
		"outputs":           map[string]any{},
		"resources": []any{
			map[string]any{
				"mode":     "managed",
				"type":     "aws_vpc",
				"name":     "main",
				"provider": "provider[\"registry.terraform.io/hashicorp/aws\"]",
				"instances": []any{
					map[string]any{
						"schema_version":       0,
						"attributes":           map[string]any{"id": "vpc-1"},
						"sensitive_attributes": []any{},
					},
				},
			},
		},
	}
	raw, _ := json.Marshal(body)
	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Second)
	defer cancel()
	if err := s.WriteState(ctx, "qtest", "", raw, "test", "test"); err != nil {
		t.Fatalf("write state: %v", err)
	}
}

func TestQuery_StreamsResults(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	mustResetTables(t, pool)
	seedSimpleState(t, s)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 10*time.Second)
	defer cancel()

	var cols []ColumnInfo
	var rows [][]any
	// Scoped to the test's own state by JOINing states. The previous
	// "SELECT name, type FROM resources" form relied on the cleanup
	// helper having TRUNCATEd the resources table; that's no longer
	// the contract (see mustResetTables for the per-state allowlist).
	err := s.Query(
		ctx,
		`SELECT r.name, r.type
		 FROM resources r
		 JOIN states s ON s.id = r.state_id
		 WHERE s.name = 'qtest'
		 ORDER BY r.name`,
		5*time.Second,
		func(c []ColumnInfo) error { cols = c; return nil },
		func(v []any) error {
			cp := make([]any, len(v))
			copy(cp, v)
			rows = append(rows, cp)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(cols) != 2 || cols[0].Name != "name" || cols[1].Name != "type" {
		t.Fatalf("unexpected columns: %+v", cols)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if rows[0][0] != "main" || rows[0][1] != "aws_vpc" {
		t.Errorf("row mismatch: %+v", rows[0])
	}
}

func TestQuery_RejectsWrites(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	mustResetTables(t, pool)
	seedSimpleState(t, s)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Second)
	defer cancel()

	err := s.Query(
		ctx,
		"DELETE FROM resources",
		5*time.Second,
		func([]ColumnInfo) error { return nil },
		func([]any) error { return nil },
	)
	if err == nil {
		t.Fatal("expected read-only error, got nil")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("expected pg error, got %v (%T)", err, err)
	}
	// SQLSTATE 25006 = read_only_sql_transaction.
	if pgErr.Code != "25006" {
		t.Errorf("expected SQLSTATE 25006, got %s (%s)", pgErr.Code, pgErr.Message)
	}
}

func TestQuery_StatementTimeoutFires(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 10*time.Second)
	defer cancel()

	err := s.Query(
		ctx,
		"SELECT pg_sleep(2)",
		100*time.Millisecond,
		func([]ColumnInfo) error { return nil },
		func([]any) error { return nil },
	)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	// Could be either a pg error with SQLSTATE 57014 (query_canceled) or
	// a wrapped form of the same. Either way the message mentions cancel
	// or timeout.
	msg := err.Error()
	if !strings.Contains(strings.ToLower(msg), "canceling") && !strings.Contains(strings.ToLower(msg), "statement timeout") {
		t.Errorf("unexpected error: %v", err)
	}
}

// mustResetTables wipes test-owned state so every integration test
// starts from a known-clean baseline — but preserves any state
// listed in the protected-states allowlist (default: "big-state",
// overridable via $KL_TEST_PROTECT_STATES). The previous
// implementation called TRUNCATE ... CASCADE on the whole schema,
// which silently destroyed the operator-managed big-state fixture
// every time the integration suite ran against a shared dev DB.
// One demo prep round of "where did big-state go?" was enough; the
// allowlist guards against that recurrence.
//
// Cleanup order:
//
//  1. Events first. The schema's ON DELETE SET NULL on
//     events.state_id means a later DELETE FROM states would
//     orphan rather than remove unprotected-state events, so we
//     remove them by membership against the protected set before
//     the cascade fires.
//
//  2. States second. Cascades through state_versions, resources,
//     outputs, state_locks, apply_runs, resource_reservations,
//     refresh_runs.
//
// Tables intentionally NOT touched: provider_configs,
// provider_schemas, schema_migrations. Those are global by
// design; if a test needs them clean it manages that itself.
func mustResetTables(t *testing.T, pool *db.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 10*time.Second)
	defer cancel()

	protected := testdb.ProtectedStates()

	if _, err := pool.Exec(ctx, `
		DELETE FROM events
		WHERE state_id IS NULL
		   OR state_id NOT IN (SELECT id FROM states WHERE name = ANY($1))
	`, protected); err != nil {
		t.Fatalf("delete unprotected events: %v", err)
	}

	if _, err := pool.Exec(ctx, `
		DELETE FROM states WHERE NOT (name = ANY($1))
	`, protected); err != nil {
		t.Fatalf("delete unprotected states: %v", err)
	}

	// Integration tests run as the well-known self-hosted CLI principal.
	// Some lifecycle tests intentionally suspend that tenant; without
	// resetting it here, later WriteState calls fail with "tenant is
	// suspended" even though the individual test fixture is otherwise
	// clean. Keep the shared self-hosted tenant active between tests.
	if _, err := pool.Exec(ctx, `
		UPDATE tenants
		SET lifecycle_status = 'active',
		    lifecycle_changed_at = now(),
		    lifecycle_changed_by = 'itest-reset',
		    lifecycle_reason = 'integration test reset',
		    max_state_resources = 0,
		    max_environment_resources = 0
		WHERE id = $1::uuid
	`, auth.SelfHostedTenantID); err != nil {
		t.Fatalf("reset self-hosted tenant policy: %v", err)
	}
}
