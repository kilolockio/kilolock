//go:build integration

// This file is built only with the `integration` build tag, which means
// `go test ./...` skips it by default. To run:
//
//	KL_DATABASE_URL=postgres://kl:kl@localhost:5432/kl?sslmode=disable \
//	  go test -tags=integration ./internal/backend/...
//
// Tests in this file expect a running Postgres with the v0 schema already
// applied (e.g. via `kl migrate`).

package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kilolockio/kilolock/pkg/auth"
	"github.com/kilolockio/kilolock/pkg/db"
	"github.com/kilolockio/kilolock/pkg/migrate"
	"github.com/kilolockio/kilolock/pkg/store"
	"github.com/kilolockio/kilolock/pkg/testdb"
)

// assertJSONEqual compares two JSON byte slices for semantic equality.
// We can't compare byte-equal because raw_state is stored as JSONB,
// which Postgres normalizes (whitespace, key ordering). The wire
// contract with Terraform is semantic JSON equality, not byte equality.
func assertJSONEqual(got, want []byte) error {
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		return fmt.Errorf("got is not valid JSON: %w", err)
	}
	if err := json.Unmarshal(want, &w); err != nil {
		return fmt.Errorf("want is not valid JSON: %w", err)
	}
	if !reflect.DeepEqual(g, w) {
		return fmt.Errorf("JSON not semantically equal")
	}
	return nil
}

func requireDB(t *testing.T) *db.Pool {
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
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

// resetTables wipes test-owned state between server-integration
// runs while preserving the operator's big-state demo fixture and
// any names listed in $KL_TEST_PROTECT_STATES. See
// internal/testdb.ProtectedStates for the policy and
// internal/store.mustResetTables for the equivalent in the store
// package — kept in lockstep on purpose.
func resetTables(t *testing.T, pool *db.Pool) {
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

func setSelfHostedTenantLifecycleStatus(t *testing.T, pool *db.Pool, status store.LifecycleStatus) {
	t.Helper()
	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 10*time.Second)
	defer cancel()

	tag, err := pool.Exec(ctx,
		`UPDATE tenants
		 SET lifecycle_status = $2
		 WHERE id = $1`,
		auth.SelfHostedTenantID, string(status),
	)
	if err != nil {
		t.Fatalf("update tenant lifecycle_status: %v", err)
	}
	if tag.RowsAffected() == 1 {
		return
	}

	// Backward-compat: some test runs may start from a DB that doesn't yet
	// have the self-hosted tenant row. Insert the minimum viable row.
	_, err = pool.Exec(ctx,
		`INSERT INTO tenants (id, slug, name, lifecycle_status)
		 VALUES ($1, 'self-hosted-bootstrap', 'Self Hosted', $2)
		 ON CONFLICT (id) DO UPDATE SET lifecycle_status = EXCLUDED.lifecycle_status`,
		auth.SelfHostedTenantID, string(status),
	)
	if err != nil {
		t.Fatalf("insert bootstrap tenant row: %v", err)
	}
}

// minimalState returns a syntactically valid Terraform v4 state with the
// given serial. The lineage is constant so tests can assert equality.
func minimalState(serial int64) []byte {
	body := map[string]any{
		"version":           4,
		"terraform_version": "1.13.4",
		"serial":            serial,
		"lineage":           "9b39e2c0-1111-2222-3333-444455556666",
		"outputs":           map[string]any{},
		"resources":         []any{},
	}
	b, _ := json.Marshal(body)
	return b
}

// stateWithGraph returns a more representative state containing two
// resources, one dependency, and one output. It exists so the
// normalization integration test has something interesting to verify.
func stateWithGraph(serial int64) []byte {
	body := map[string]any{
		"version":           4,
		"terraform_version": "1.13.4",
		"serial":            serial,
		"lineage":           "9b39e2c0-1111-2222-3333-444455556666",
		"outputs": map[string]any{
			"vpc_id": map[string]any{
				"value": "vpc-123",
				"type":  "string",
			},
		},
		"resources": []any{
			map[string]any{
				"mode":     "managed",
				"type":     "aws_vpc",
				"name":     "main",
				"provider": "provider[\"registry.terraform.io/hashicorp/aws\"]",
				"instances": []any{
					map[string]any{
						"schema_version":       0,
						"attributes":           map[string]any{"id": "vpc-123", "cidr_block": "10.0.0.0/16"},
						"sensitive_attributes": []any{},
					},
				},
			},
			map[string]any{
				"mode":     "managed",
				"type":     "aws_subnet",
				"name":     "private",
				"provider": "provider[\"registry.terraform.io/hashicorp/aws\"]",
				"instances": []any{
					map[string]any{
						"schema_version":       0,
						"attributes":           map[string]any{"id": "subnet-1"},
						"sensitive_attributes": []any{},
						"dependencies":         []string{"aws_vpc.main"},
						"index_key":            0,
					},
				},
			},
		},
	}
	b, _ := json.Marshal(body)
	return b
}

func TestEndToEnd_PutGetDelete(t *testing.T) {
	pool := requireDB(t)
	t.Cleanup(pool.Close)
	resetTables(t, pool)
	setSelfHostedTenantLifecycleStatus(t, pool, store.LifecycleStatusActive)

	srv := New(store.New(pool.Pool), slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := srv.Handler()

	state := "smoke"

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/states/"+state, nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("initial GET status = %d, want 404", w.Code)
	}

	body := minimalState(1)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/states/"+state, bytes.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("POST without lock status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/states/"+state, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET after POST status = %d, want 200", w.Code)
	}
	if err := assertJSONEqual(w.Body.Bytes(), body); err != nil {
		t.Fatalf("round-trip mismatch: %v\n got: %s\nwant: %s", err, w.Body.String(), string(body))
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/v1/states/"+state, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d, want 200", w.Code)
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/states/"+state, nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("GET after DELETE status = %d, want 404", w.Code)
	}
}

func TestEndToEnd_PutGetDelete_MultiSegmentStateName(t *testing.T) {
	pool := requireDB(t)
	t.Cleanup(pool.Close)
	resetTables(t, pool)
	setSelfHostedTenantLifecycleStatus(t, pool, store.LifecycleStatusActive)

	srv := New(store.New(pool.Pool), slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := srv.Handler()

	state := "vnv/prod/my_awesome_project"

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/states/"+state, nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("initial GET status = %d, want 404", w.Code)
	}

	body := minimalState(1)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/states/"+state, bytes.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("POST without lock status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/states/"+state, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET after POST status = %d, want 200", w.Code)
	}
	if err := assertJSONEqual(w.Body.Bytes(), body); err != nil {
		t.Fatalf("round-trip mismatch: %v\n got: %s\nwant: %s", err, w.Body.String(), string(body))
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/v1/states/"+state, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d, want 200", w.Code)
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/states/"+state, nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("GET after DELETE status = %d, want 404", w.Code)
	}
}

func TestEndToEnd_LockLifecycle(t *testing.T) {
	pool := requireDB(t)
	t.Cleanup(pool.Close)
	resetTables(t, pool)
	setSelfHostedTenantLifecycleStatus(t, pool, store.LifecycleStatusActive)

	srv := New(store.New(pool.Pool), slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := srv.Handler()

	state := "lockfun"
	info := store.LockInfo{
		ID:        "lock-aaa",
		Operation: "OperationTypeApply",
		Who:       "alice@laptop",
		Version:   "1.13.4",
		Created:   time.Now().UTC().Format(time.RFC3339Nano),
		Path:      "kl://v1/states/" + state,
	}
	infoJSON, _ := json.Marshal(info)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("LOCK", "/v1/states/"+state, bytes.NewReader(infoJSON)))
	if w.Code != http.StatusOK {
		t.Fatalf("first LOCK status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}

	// Migration 0011 made HTTP-backend locks optimistic by default:
	// the lock channel only carries a generic LockInfo (no
	// resource-level data) so we can't enforce a useful conflict at
	// LOCK time. Conflict detection moved to POST. To preserve the
	// legacy 423-on-conflict semantics this test asserts, flip the
	// state into exclusive mode before continuing.
	if _, err := pool.Exec(context.Background(),
		`UPDATE states SET exclusive_locks = true WHERE name = $1`,
		state,
	); err != nil {
		t.Fatalf("flip exclusive_locks: %v", err)
	}

	conflict := info
	conflict.ID = "lock-bbb"
	conflict.Who = "bob@laptop"
	conflictJSON, _ := json.Marshal(conflict)

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("LOCK", "/v1/states/"+state, bytes.NewReader(conflictJSON)))
	if w.Code != http.StatusLocked {
		t.Fatalf("conflicting LOCK status = %d, want 423", w.Code)
	}
	var got store.LockInfo
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("conflict body not LockInfo JSON: %v", err)
	}
	if got.ID != info.ID {
		t.Errorf("conflict body ID = %q, want %q", got.ID, info.ID)
	}

	body := minimalState(1)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/states/"+state, bytes.NewReader(body)))
	if w.Code != http.StatusLocked {
		t.Errorf("POST without lock id while locked: status = %d, want 423", w.Code)
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/states/"+state+"?ID=wrong", bytes.NewReader(body)))
	if w.Code != http.StatusConflict {
		t.Errorf("POST with wrong lock id: status = %d, want 409", w.Code)
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/states/"+state+"?ID="+info.ID, bytes.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Errorf("POST with correct lock id: status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}

	wrong := info
	wrong.ID = "wrong"
	wrongJSON, _ := json.Marshal(wrong)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("UNLOCK", "/v1/states/"+state, bytes.NewReader(wrongJSON)))
	if w.Code != http.StatusConflict {
		t.Errorf("UNLOCK with wrong id: status = %d, want 409", w.Code)
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("UNLOCK", "/v1/states/"+state, bytes.NewReader(infoJSON)))
	if w.Code != http.StatusOK {
		t.Errorf("UNLOCK with correct id: status = %d, want 200", w.Code)
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/states/"+state, bytes.NewReader(minimalState(2))))
	if w.Code != http.StatusOK {
		t.Errorf("POST without lock after UNLOCK: status = %d, want 200", w.Code)
	}
}

func TestEndToEnd_NormalizationProjectsRowsAndEdges(t *testing.T) {
	pool := requireDB(t)
	t.Cleanup(pool.Close)
	resetTables(t, pool)
	setSelfHostedTenantLifecycleStatus(t, pool, store.LifecycleStatusActive)

	srv := New(store.New(pool.Pool), slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := srv.Handler()

	state := "graph"
	body := stateWithGraph(1)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/states/"+state, bytes.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("POST status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 10*time.Second)
	defer cancel()

	var resourceCount int
	err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM resources r
		JOIN   states s ON s.id = r.state_id
		WHERE  s.name = $1
		  AND  r.delete_serial IS NULL
	`, state).Scan(&resourceCount)
	if err != nil {
		t.Fatalf("query resources: %v", err)
	}
	if resourceCount != 2 {
		t.Errorf("resource count = %d, want 2", resourceCount)
	}

	// resource_dependencies is now a VIEW filtered per state_version;
	// scope it explicitly to the current version.
	var edgeCount int
	err = pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM   resource_dependencies rd
		JOIN   states s ON s.current_version_id = rd.state_version_id
		WHERE  s.name = $1
	`, state).Scan(&edgeCount)
	if err != nil {
		t.Fatalf("query edges: %v", err)
	}
	if edgeCount != 1 {
		t.Errorf("edge count = %d, want 1 (subnet -> vpc)", edgeCount)
	}

	var outputCount int
	err = pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM outputs o
		JOIN   states s ON s.current_version_id = o.state_version_id
		WHERE  s.name = $1
	`, state).Scan(&outputCount)
	if err != nil {
		t.Fatalf("query outputs: %v", err)
	}
	if outputCount != 1 {
		t.Errorf("output count = %d, want 1", outputCount)
	}

	// Round-trip via the HTTP GET path must be semantically identical
	// to what we POSTed. We don't promise byte equality because
	// raw_state is stored as JSONB, which Postgres normalizes
	// (whitespace, key ordering). Terraform's HTTP backend client
	// re-parses the JSON either way, so semantic equality is the
	// contract.
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/states/"+state, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", w.Code)
	}
	if err := assertJSONEqual(w.Body.Bytes(), body); err != nil {
		t.Errorf("round-trip mismatch: %v\n got: %s\nwant: %s", err, w.Body.String(), string(body))
	}
}

// TestEndToEnd_ForceUnlock_EmptyBody simulates what Terraform's
// `terraform force-unlock` actually sends to an http backend: a UNLOCK
// request with an empty body (because client.jsonLockInfo is unset
// outside the same process that took the lock). The handler must
// release whatever lock is held and record a `lock_force_release`
// event in the audit trail.
func TestEndToEnd_ForceUnlock_EmptyBody(t *testing.T) {
	pool := requireDB(t)
	t.Cleanup(pool.Close)
	resetTables(t, pool)
	setSelfHostedTenantLifecycleStatus(t, pool, store.LifecycleStatusActive)

	srv := New(store.New(pool.Pool), slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := srv.Handler()

	state := "force-unlock"
	info := store.LockInfo{
		ID:        "lock-original",
		Operation: "OperationTypeApply",
		Who:       "alice@laptop",
		Version:   "1.13.4",
		Created:   time.Now().UTC().Format(time.RFC3339Nano),
		Path:      "kl://v1/states/" + state,
	}
	infoJSON, _ := json.Marshal(info)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("LOCK", "/v1/states/"+state, bytes.NewReader(infoJSON)))
	if w.Code != http.StatusOK {
		t.Fatalf("initial LOCK status = %d, want 200", w.Code)
	}

	// Empty-body UNLOCK -- this is the wire shape terraform force-unlock
	// produces. Must succeed regardless of which ID Bob thinks the lock has.
	req := httptest.NewRequest("UNLOCK", "/v1/states/"+state, http.NoBody)
	req.Header.Set("User-Agent", "Terraform/1.13.4")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("force-unlock (empty body) status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}

	// Lock row should be gone.
	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Second)
	defer cancel()
	var locks int
	if err := pool.QueryRow(ctx,
		`SELECT count(*)::int FROM state_locks l JOIN states s ON s.id=l.state_id WHERE s.name=$1`,
		state,
	).Scan(&locks); err != nil {
		t.Fatalf("count locks: %v", err)
	}
	if locks != 0 {
		t.Errorf("locks after force-unlock = %d, want 0", locks)
	}

	// Audit event recorded with the original lock_id payload.
	var kind, lockID string
	err := pool.QueryRow(ctx,
		`SELECT e.kind, e.payload->>'lock_id'
		 FROM   events e JOIN states s ON s.id=e.state_id
		 WHERE  s.name=$1
		 ORDER  BY e.created_at DESC
		 LIMIT  1`,
		state,
	).Scan(&kind, &lockID)
	if err != nil {
		t.Fatalf("audit query: %v", err)
	}
	if kind != "lock_force_release" {
		t.Errorf("last event kind = %q, want lock_force_release", kind)
	}
	if lockID != info.ID {
		t.Errorf("audit lock_id = %q, want %q", lockID, info.ID)
	}

	// Second force-unlock should be a no-op success (idempotent).
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("UNLOCK", "/v1/states/"+state, http.NoBody))
	if w.Code != http.StatusOK {
		t.Errorf("second force-unlock status = %d, want 200", w.Code)
	}
}

// TestEndToEnd_ForceUnlock_EmptyJSON covers the defensive branch:
// some clients send `{}` rather than truly empty bodies for
// force-unlock. Behavior should be identical to the empty-body case.
func TestEndToEnd_ForceUnlock_EmptyJSON(t *testing.T) {
	pool := requireDB(t)
	t.Cleanup(pool.Close)
	resetTables(t, pool)
	setSelfHostedTenantLifecycleStatus(t, pool, store.LifecycleStatusActive)

	srv := New(store.New(pool.Pool), slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := srv.Handler()

	state := "force-unlock-json"
	info := store.LockInfo{ID: "lock-empty-json", Who: "alice", Operation: "OperationTypeApply"}
	infoJSON, _ := json.Marshal(info)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("LOCK", "/v1/states/"+state, bytes.NewReader(infoJSON)))
	if w.Code != http.StatusOK {
		t.Fatalf("LOCK status = %d, want 200", w.Code)
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("UNLOCK", "/v1/states/"+state, bytes.NewReader([]byte("{}"))))
	if w.Code != http.StatusOK {
		t.Errorf("UNLOCK with empty JSON status = %d, want 200", w.Code)
	}
}

func TestEndToEnd_Unlock_PlainStringID(t *testing.T) {
	pool := requireDB(t)
	t.Cleanup(pool.Close)
	resetTables(t, pool)
	setSelfHostedTenantLifecycleStatus(t, pool, store.LifecycleStatusActive)

	srv := New(store.New(pool.Pool), slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := srv.Handler()

	state := "unlock-plain-id"
	info := store.LockInfo{
		ID:        "lock-plain-id",
		Operation: "OperationTypeApply",
		Who:       "alice@laptop",
		Version:   "1.13.4",
		Created:   time.Now().UTC().Format(time.RFC3339Nano),
		Path:      "kl://v1/states/" + state,
	}
	infoJSON, _ := json.Marshal(info)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("LOCK", "/v1/states/"+state, bytes.NewReader(infoJSON)))
	if w.Code != http.StatusOK {
		t.Fatalf("LOCK status = %d, want 200", w.Code)
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("UNLOCK", "/v1/states/"+state, strings.NewReader(info.ID)))
	if w.Code != http.StatusOK {
		t.Fatalf("UNLOCK with plain string id status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
}

func TestEndToEnd_Unlock_JSONStringID(t *testing.T) {
	pool := requireDB(t)
	t.Cleanup(pool.Close)
	resetTables(t, pool)
	setSelfHostedTenantLifecycleStatus(t, pool, store.LifecycleStatusActive)

	srv := New(store.New(pool.Pool), slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := srv.Handler()

	state := "unlock-json-string-id"
	info := store.LockInfo{
		ID:        "lock-json-string-id",
		Operation: "OperationTypeApply",
		Who:       "alice@laptop",
		Version:   "1.13.4",
		Created:   time.Now().UTC().Format(time.RFC3339Nano),
		Path:      "kl://v1/states/" + state,
	}
	infoJSON, _ := json.Marshal(info)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("LOCK", "/v1/states/"+state, bytes.NewReader(infoJSON)))
	if w.Code != http.StatusOK {
		t.Fatalf("LOCK status = %d, want 200", w.Code)
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("UNLOCK", "/v1/states/"+state, strings.NewReader(`"`+info.ID+`"`)))
	if w.Code != http.StatusOK {
		t.Fatalf("UNLOCK with JSON string id status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
}

func TestEndToEnd_Unlock_QueryIDWithoutBody(t *testing.T) {
	pool := requireDB(t)
	t.Cleanup(pool.Close)
	resetTables(t, pool)
	setSelfHostedTenantLifecycleStatus(t, pool, store.LifecycleStatusActive)

	srv := New(store.New(pool.Pool), slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := srv.Handler()

	state := "unlock-query-id"
	info := store.LockInfo{
		ID:        "lock-query-id",
		Operation: "OperationTypeApply",
		Who:       "alice@laptop",
		Version:   "1.13.4",
		Created:   time.Now().UTC().Format(time.RFC3339Nano),
		Path:      "kl://v1/states/" + state,
	}
	infoJSON, _ := json.Marshal(info)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("LOCK", "/v1/states/"+state, bytes.NewReader(infoJSON)))
	if w.Code != http.StatusOK {
		t.Fatalf("LOCK status = %d, want 200", w.Code)
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("UNLOCK", "/v1/states/"+state+"?ID="+info.ID, http.NoBody))
	if w.Code != http.StatusOK {
		t.Fatalf("UNLOCK with query id status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
}

// TestEndToEnd_ForceUnlock_NoLockHeld confirms that force-unlock is
// idempotent on a state with no held lock (and even on a state that
// doesn't exist yet).
func TestEndToEnd_ForceUnlock_NoLockHeld(t *testing.T) {
	pool := requireDB(t)
	t.Cleanup(pool.Close)
	resetTables(t, pool)
	setSelfHostedTenantLifecycleStatus(t, pool, store.LifecycleStatusActive)

	srv := New(store.New(pool.Pool), slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := srv.Handler()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("UNLOCK", "/v1/states/does-not-exist", http.NoBody))
	if w.Code != http.StatusOK {
		t.Errorf("force-unlock against nonexistent state status = %d, want 200", w.Code)
	}
}

// TestEndToEnd_Unlock_OwnerReleasePreserved regression-checks that the
// pre-existing matched-ID release path still works, including the
// wrong-ID 409 response.
func TestEndToEnd_Unlock_OwnerReleasePreserved(t *testing.T) {
	pool := requireDB(t)
	t.Cleanup(pool.Close)
	resetTables(t, pool)
	setSelfHostedTenantLifecycleStatus(t, pool, store.LifecycleStatusActive)

	srv := New(store.New(pool.Pool), slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := srv.Handler()

	state := "owner-release"
	info := store.LockInfo{ID: "lock-owner", Who: "alice", Operation: "OperationTypeApply"}
	infoJSON, _ := json.Marshal(info)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("LOCK", "/v1/states/"+state, bytes.NewReader(infoJSON)))
	if w.Code != http.StatusOK {
		t.Fatalf("LOCK status = %d, want 200", w.Code)
	}

	wrong := info
	wrong.ID = "not-the-real-id"
	wrongJSON, _ := json.Marshal(wrong)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("UNLOCK", "/v1/states/"+state, bytes.NewReader(wrongJSON)))
	if w.Code != http.StatusConflict {
		t.Errorf("UNLOCK with wrong ID status = %d, want 409", w.Code)
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("UNLOCK", "/v1/states/"+state, bytes.NewReader(infoJSON)))
	if w.Code != http.StatusOK {
		t.Errorf("UNLOCK with correct ID status = %d, want 200", w.Code)
	}

	// Audit event should be a regular lock_release, not lock_force_release.
	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Second)
	defer cancel()
	var kind string
	err := pool.QueryRow(ctx,
		`SELECT e.kind
		 FROM   events e JOIN states s ON s.id=e.state_id
		 WHERE  s.name=$1
		 ORDER  BY e.created_at DESC
		 LIMIT  1`,
		state,
	).Scan(&kind)
	if err != nil {
		t.Fatalf("audit query: %v", err)
	}
	if kind != "lock_release" {
		t.Errorf("last event kind = %q, want lock_release", kind)
	}
}

// TestEndToEnd_Lifecycle_ResourceChangeClosesAndReopens covers the
// signature lifecycle semantics: when a resource's attributes change
// between two writes, the old `resources` row is closed
// (delete_serial = new serial) and a new row is opened with the new
// hash. The new row is visible in current_resources; the old row is
// not.
//
// Also covers removal: a resource present in version N but absent in
// version N+1 must have its lifecycle closed.
func TestEndToEnd_Lifecycle_ResourceChangeClosesAndReopens(t *testing.T) {
	pool := requireDB(t)
	t.Cleanup(pool.Close)
	resetTables(t, pool)
	setSelfHostedTenantLifecycleStatus(t, pool, store.LifecycleStatusActive)

	srv := New(store.New(pool.Pool), slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := srv.Handler()
	state := "lifecycle"

	mkState := func(serial int64, vpcCIDR string, includeSubnet bool) []byte {
		resources := []any{
			map[string]any{
				"mode":     "managed",
				"type":     "aws_vpc",
				"name":     "main",
				"provider": "provider[\"registry.terraform.io/hashicorp/aws\"]",
				"instances": []any{
					map[string]any{
						"schema_version":       0,
						"attributes":           map[string]any{"id": "vpc-1", "cidr_block": vpcCIDR},
						"sensitive_attributes": []any{},
					},
				},
			},
		}
		if includeSubnet {
			resources = append(resources, map[string]any{
				"mode":     "managed",
				"type":     "aws_subnet",
				"name":     "private",
				"provider": "provider[\"registry.terraform.io/hashicorp/aws\"]",
				"instances": []any{
					map[string]any{
						"schema_version":       0,
						"attributes":           map[string]any{"id": "subnet-1"},
						"sensitive_attributes": []any{},
					},
				},
			})
		}
		body, _ := json.Marshal(map[string]any{
			"version":           4,
			"terraform_version": "1.13.4",
			"serial":            serial,
			"lineage":           "9b39e2c0-aaaa-bbbb-cccc-dddddddddddd",
			"outputs":           map[string]any{},
			"resources":         resources,
		})
		return body
	}

	post := func(serial int64, body []byte) {
		t.Helper()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/states/"+state, bytes.NewReader(body)))
		if w.Code != http.StatusOK {
			t.Fatalf("POST serial=%d status=%d body=%s", serial, w.Code, w.Body.String())
		}
	}

	post(1, mkState(1, "10.0.0.0/16", true))
	post(2, mkState(2, "10.0.0.0/16", true))  // unchanged
	post(3, mkState(3, "10.1.0.0/16", true))  // vpc cidr changed -> close + reopen vpc row
	post(4, mkState(4, "10.1.0.0/16", false)) // subnet removed -> close subnet row

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Second)
	defer cancel()

	type row struct {
		address      string
		createSerial int64
		deleteSerial *int64
		hash         string
	}
	var rows []row
	r, err := pool.Query(ctx, `
		SELECT r.address, r.create_serial, r.delete_serial, r.attributes_hash
		FROM   resources r
		JOIN   states s ON s.id = r.state_id
		WHERE  s.name = $1
		ORDER  BY r.address, r.create_serial
	`, state)
	if err != nil {
		t.Fatalf("query resources: %v", err)
	}
	defer r.Close()
	for r.Next() {
		var v row
		if err := r.Scan(&v.address, &v.createSerial, &v.deleteSerial, &v.hash); err != nil {
			t.Fatalf("scan: %v", err)
		}
		rows = append(rows, v)
	}

	// Expected lifecycle: vpc has two ranges, subnet has one closed range.
	//   aws_subnet.private: create=1, delete=4   (lived 1..3, closed in 4)
	//   aws_vpc.main      : create=1, delete=3   (cidr 10.0.0.0/16, closed in 3)
	//   aws_vpc.main      : create=3, delete=NULL (cidr 10.1.0.0/16, still open at 4)
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3: %#v", len(rows), rows)
	}

	// aws_subnet.private (one row)
	if rows[0].address != "aws_subnet.private" || rows[0].createSerial != 1 ||
		rows[0].deleteSerial == nil || *rows[0].deleteSerial != 4 {
		t.Errorf("subnet row = %#v, want create=1 delete=4", rows[0])
	}
	// aws_vpc.main first lifecycle (closed)
	if rows[1].address != "aws_vpc.main" || rows[1].createSerial != 1 ||
		rows[1].deleteSerial == nil || *rows[1].deleteSerial != 3 {
		t.Errorf("vpc closed row = %#v, want create=1 delete=3", rows[1])
	}
	// aws_vpc.main second lifecycle (open)
	if rows[2].address != "aws_vpc.main" || rows[2].createSerial != 3 ||
		rows[2].deleteSerial != nil {
		t.Errorf("vpc open row = %#v, want create=3 delete=NULL", rows[2])
	}

	// Hashes for the two vpc rows must differ (cidr changed).
	if rows[1].hash == rows[2].hash {
		t.Errorf("vpc rows have identical attributes_hash but cidr_block changed")
	}

	// current_resources view should show exactly one resource (the open vpc).
	var openCount int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM current_resources WHERE state_name = $1`, state).Scan(&openCount); err != nil {
		t.Fatalf("current_resources: %v", err)
	}
	if openCount != 1 {
		t.Errorf("current_resources count = %d, want 1", openCount)
	}

	// Point-in-time check: at serial=2 (both resources present, unchanged),
	// resource_dependencies / current view of the world should be 2.
	var at2 int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM resources r
		JOIN states s ON s.id = r.state_id
		WHERE  s.name = $1
		  AND  r.create_serial <= 2
		  AND  (r.delete_serial IS NULL OR r.delete_serial > 2)
	`, state).Scan(&at2); err != nil {
		t.Fatalf("point-in-time query: %v", err)
	}
	if at2 != 2 {
		t.Errorf("point-in-time at serial=2 count = %d, want 2", at2)
	}
}

// TestEndToEnd_SerialAutoBumped pins the new optimistic-mode
// contract: the HTTP backend recomputes state_versions.serial to
// MAX+1 on every POST, so a POSTed body whose embedded "serial"
// field happens to collide with an existing version does NOT
// reject. Each successful POST advances trunk regardless.
//
// The legacy 409-on-duplicate-serial behavior was a side-effect of
// trusting the operator's serial; it stopped being a useful
// invariant once multiple operators can hold concurrent locks and
// each independently computes "serial = my-base + 1." Conflict
// detection moved to the write-set layer (see
// WriteSetConflictError); serial is now an internal monotone
// counter the backend assigns.
//
// To restore the legacy behavior, set states.exclusive_locks=true:
// the exclusive path still recomputes serial too (consistency
// across paths), but the lock check prevents the second POST from
// reaching that code in the first place.
func TestEndToEnd_SerialAutoBumped(t *testing.T) {
	pool := requireDB(t)
	t.Cleanup(pool.Close)
	resetTables(t, pool)
	setSelfHostedTenantLifecycleStatus(t, pool, store.LifecycleStatusActive)

	srv := New(store.New(pool.Pool), slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := srv.Handler()

	state := "serial"
	body := minimalState(5)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/states/"+state, bytes.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("first POST status = %d, want 200", w.Code)
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/states/"+state, bytes.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Errorf("repeat POST status = %d, want 200 (serial auto-bumped)", w.Code)
	}

	// Both writes should have produced distinct state_versions
	// rows, with the backend's internal serial advancing.
	var got []int64
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := pool.Query(ctx,
		`SELECT serial FROM state_versions sv
		 JOIN   states s ON s.id = sv.state_id
		 WHERE  s.name = $1
		 ORDER BY serial`,
		state,
	)
	if err != nil {
		t.Fatalf("query state_versions: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var sv int64
		if err := rows.Scan(&sv); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, sv)
	}
	if len(got) != 2 || got[0] >= got[1] {
		t.Errorf("state_versions serials = %v, want two strictly increasing entries", got)
	}
}

func TestEndToEnd_TenantSuspended_BlocksMutations(t *testing.T) {
	pool := requireDB(t)
	t.Cleanup(pool.Close)
	resetTables(t, pool)
	setSelfHostedTenantLifecycleStatus(t, pool, store.LifecycleStatusSuspended)

	srv := New(store.New(pool.Pool), slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := srv.Handler()

	state := "tenant-suspended"

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/states/"+state, bytes.NewReader(minimalState(1))))
	if w.Code != http.StatusForbidden {
		t.Fatalf("POST status = %d, want 403 (body=%s)", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/v1/states/"+state, nil))
	if w.Code != http.StatusForbidden && w.Code != http.StatusNotFound {
		// Either is acceptable: the lifecycle check may run before state lookup,
		// so DELETE is forbidden even for missing state.
		t.Fatalf("DELETE status = %d, want 403 or 404 (body=%s)", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("LOCK", "/v1/states/"+state, bytes.NewBufferString(`{"ID":"lock-1","Operation":"OperationTypeApply"}`)))
	if w.Code != http.StatusForbidden {
		t.Fatalf("LOCK status = %d, want 403 (body=%s)", w.Code, w.Body.String())
	}
}

func TestEndToEnd_QuotaExceeded_ReturnsReadableForbidden(t *testing.T) {
	pool := requireDB(t)
	t.Cleanup(pool.Close)
	resetTables(t, pool)

	ctx := testdb.BackgroundTenantCtx()
	if _, err := pool.Exec(ctx, `UPDATE tenants SET max_state_resources = 1, max_environment_resources = 15000 WHERE id = $1`, auth.SelfHostedTenantID); err != nil {
		t.Fatalf("set tenant quotas: %v", err)
	}

	body, _ := json.Marshal(map[string]any{
		"version":           4,
		"terraform_version": "1.13.4",
		"serial":            1,
		"lineage":           "9b39e2c0-1111-2222-3333-444455556666",
		"outputs":           map[string]any{},
		"resources": []any{
			map[string]any{
				"mode":     "managed",
				"type":     "null_resource",
				"name":     "x",
				"provider": "provider[\"registry.terraform.io/hashicorp/null\"]",
				"instances": []any{
					map[string]any{"schema_version": 0, "attributes": map[string]any{"id": "n1"}},
					map[string]any{"schema_version": 0, "attributes": map[string]any{"id": "n2"}},
					map[string]any{"schema_version": 0, "attributes": map[string]any{"id": "n3"}},
				},
			},
		},
	})

	srv := New(store.New(pool.Pool), slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := srv.Handler()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/states/quota-hit", bytes.NewReader(body)))
	if w.Code != http.StatusForbidden {
		t.Fatalf("POST status = %d, want 403 (body=%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "state quota exceeded") || !strings.Contains(w.Body.String(), "soft=") || !strings.Contains(w.Body.String(), "hard=") {
		t.Fatalf("quota error body = %q, want clearer state quota details", w.Body.String())
	}
}
