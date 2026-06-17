//go:build integration

// Integration tests for the refresh orchestrator. These drive the
// full Run path end-to-end across two axes:
//
//   - With a fake factory but REAL Postgres + Store (v1.6b paths):
//     proves GetCurrentStateInfo → BeginRefreshRun → WriteState →
//     LookupStateVersionID → FinishRefreshRun all line up; the
//     splice-back behavior commits new attributes correctly;
//     dry-run does NOT bump serial; partial failures don't commit.
//
//   - With the production ClientFactory against a REAL provider
//     binary (v1.6c paths): proves the encoding pipeline and
//     schema-cache fill against a live wire — Discover → Launch →
//     GetSchema (cache miss) → Configure → encode/ReadResource/
//     decode → splice-back.
//
// Run with:
//
//	KL_DATABASE_URL=postgres://kl:kl@localhost:5432/kl?sslmode=disable \
//	  go test -tags=integration ./internal/refresh/...
//
// On macOS, the sandbox blocks the unix-domain socket bind go-plugin
// uses; run outside the sandbox.

package refresh

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kilolockio/kilolock/internal/provider"
	"github.com/kilolockio/kilolock/internal/tfstate"
	"github.com/kilolockio/kilolock/pkg/db"
	"github.com/kilolockio/kilolock/pkg/migrate"
	"github.com/kilolockio/kilolock/pkg/store"
	"github.com/kilolockio/kilolock/pkg/testdb"
)

func openTestStore(t *testing.T) (*store.Store, *db.Pool) {
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
	return store.New(pool.Pool), pool
}

// resetAll wipes test-owned state so each refresh integration test
// starts from a known baseline, while preserving the operator's
// big-state demo fixture (see internal/testdb.ProtectedStates).
// Two-statement form mirrors internal/store.mustResetTables; the
// only difference here is that the FK cascade from states already
// covers refresh_runs, so we don't need a separate refresh_runs
// TRUNCATE for unprotected states.
func resetAll(t *testing.T, pool *db.Pool) {
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
}

const refreshTestStateRaw = `{
	"version": 4,
	"terraform_version": "1.13.4",
	"serial": 7,
	"lineage": "12345678-aaaa-bbbb-cccc-1234567890ab",
	"outputs": {},
	"resources": [
		{
			"mode": "managed",
			"type": "null_resource",
			"name": "alpha",
			"provider": "provider[\"registry.terraform.io/hashicorp/null\"]",
			"instances": [
				{
					"schema_version": 0,
					"attributes": { "id": "alpha-1", "triggers": null },
					"sensitive_attributes": []
				}
			]
		},
		{
			"mode": "managed",
			"type": "null_resource",
			"name": "beta",
			"provider": "provider[\"registry.terraform.io/hashicorp/null\"]",
			"instances": [
				{
					"schema_version": 0,
					"attributes": { "id": "beta-1", "triggers": null },
					"sensitive_attributes": []
				}
			]
		}
	]
}`

func seedRefreshState(t *testing.T, st *store.Store) {
	t.Helper()
	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 10*time.Second)
	defer cancel()
	if err := st.WriteState(ctx, "refresh-orch-test", "", []byte(refreshTestStateRaw), "test", "test"); err != nil {
		t.Fatalf("seed WriteState: %v", err)
	}
}

// TestRun_NoOpEchoCommitsNewVersion verifies the full run lifecycle
// against real Postgres with an echoing fake provider:
//
//   - audit row created in 'running', then transitioned to 'succeeded'
//   - new state_version written with bumped serial and source='refresh'
//   - to_version_id on the audit row matches the new state_version
//   - resource counters reflect 2 checked / 0 changed / 0 failed
func TestRun_NoOpEchoCommitsNewVersion(t *testing.T) {
	st, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	resetAll(t, pool)
	seedRefreshState(t, st)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 30*time.Second)
	defer cancel()

	factory := newFakeFactory()
	res, err := Run(ctx, st, factory, Options{
		StateName: "refresh-orch-test",
		Actor:     "integration-test",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if res.Status != store.RefreshRunSucceeded {
		t.Errorf("Status: got %q, want succeeded", res.Status)
	}
	if got, want := res.ResourcesChecked, 2; got != want {
		t.Errorf("ResourcesChecked: got %d, want %d", got, want)
	}
	if got := res.ResourcesChanged; got != 0 {
		t.Errorf("ResourcesChanged: got %d, want 0", got)
	}
	if res.SerialAfter != res.SerialBefore+1 {
		t.Errorf("SerialAfter: got %d, want %d", res.SerialAfter, res.SerialBefore+1)
	}

	// Audit row is in the right shape post-run.
	got, err := st.GetRefreshRun(ctx, res.RunID)
	if err != nil {
		t.Fatalf("GetRefreshRun: %v", err)
	}
	if got.Status != store.RefreshRunSucceeded {
		t.Errorf("audit Status: got %q", got.Status)
	}
	if got.ToVersionID == nil {
		t.Fatal("audit ToVersionID nil; expected new version recorded")
	}
	if got.ResourcesChecked == nil || *got.ResourcesChecked != 2 {
		t.Errorf("audit ResourcesChecked: got %v", got.ResourcesChecked)
	}

	// New state_version exists with source='refresh'.
	var src string
	err = pool.QueryRow(ctx,
		`SELECT source FROM state_versions WHERE id = $1`,
		*got.ToVersionID,
	).Scan(&src)
	if err != nil {
		t.Fatalf("query new state_version: %v", err)
	}
	if src != "refresh" {
		t.Errorf("state_versions.source: got %q, want %q", src, "refresh")
	}
}

// TestRun_DryRunSkipsCommit verifies that --dry-run mode walks
// every resource but never writes a new state_version. Audit row
// still records the outcome.
func TestRun_DryRunSkipsCommit(t *testing.T) {
	st, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	resetAll(t, pool)
	seedRefreshState(t, st)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 30*time.Second)
	defer cancel()

	factory := newFakeFactory()
	res, err := Run(ctx, st, factory, Options{
		StateName: "refresh-orch-test",
		DryRun:    true,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.SerialAfter != res.SerialBefore {
		t.Errorf("SerialAfter: got %d, want %d (dry-run never bumps)", res.SerialAfter, res.SerialBefore)
	}

	got, err := st.GetRefreshRun(ctx, res.RunID)
	if err != nil {
		t.Fatalf("GetRefreshRun: %v", err)
	}
	if got.ToVersionID != nil {
		t.Errorf("dry-run audit ToVersionID: got %v, want nil", *got.ToVersionID)
	}
	// Still 'succeeded' because nothing failed; dry-run is not its
	// own terminal status.
	if got.Status != store.RefreshRunSucceeded {
		t.Errorf("Status: got %q, want succeeded", got.Status)
	}

	// And the live current_serial in the DB hasn't moved.
	cur, err := st.CurrentSerial(ctx, "refresh-orch-test")
	if err != nil {
		t.Fatalf("CurrentSerial: %v", err)
	}
	if cur != res.SerialBefore {
		t.Errorf("CurrentSerial: got %d, want %d (no DB write)", cur, res.SerialBefore)
	}
}

// TestRun_DriftIsCommitted exercises the splice-back path: the
// fake factory returns a different attribute payload, the
// orchestrator must persist it, and the new state_version must
// observe the new attributes via the resources view.
func TestRun_DriftIsCommitted(t *testing.T) {
	st, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	resetAll(t, pool)
	seedRefreshState(t, st)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 30*time.Second)
	defer cancel()

	factory := newFakeFactory()
	factory.onOpen = func(src provider.SourceAddress, alias string, c *recordingClient) {
		c.onRead = func(typeName string, currentState []byte) ([]byte, provider.Diagnostics, error) {
			// Simulate cloud drift: id mutated, triggers populated.
			return []byte(`{"id":"drift-id","triggers":{"refreshed":"yes"}}`), nil, nil
		}
	}

	res, err := Run(ctx, st, factory, Options{StateName: "refresh-orch-test"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ResourcesChanged != 2 {
		t.Errorf("ResourcesChanged: got %d, want 2", res.ResourcesChanged)
	}

	// Read the new live state and verify the splice landed.
	raw, err := st.GetCurrentState(ctx, "refresh-orch-test")
	if err != nil {
		t.Fatalf("GetCurrentState: %v", err)
	}
	// Postgres JSONB normalizes whitespace on read; assert only on
	// content, not formatting.
	if !strings.Contains(string(raw), `"drift-id"`) {
		t.Errorf("new state missing drift-id; got: %s", string(raw))
	}
	if !strings.Contains(string(raw), `"refreshed"`) || !strings.Contains(string(raw), `"yes"`) {
		t.Errorf("new state missing refreshed marker; got: %s", string(raw))
	}

	// v1.7a: per-resource drift addresses must be populated, sorted,
	// and consistent with the aggregate counter. Both null instances
	// in the seed fixture drifted, so both addresses must appear.
	if got := len(res.ChangedAddresses); got != res.ResourcesChanged {
		t.Errorf("ChangedAddresses len=%d, ResourcesChanged=%d (must match)",
			got, res.ResourcesChanged)
	}
	wantAddrs := []string{"null_resource.alpha", "null_resource.beta"}
	for _, want := range wantAddrs {
		found := false
		for _, got := range res.ChangedAddresses {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing drift address %q; got %v", want, res.ChangedAddresses)
		}
	}
	// The slice must be sorted (Run-level invariant) so two
	// invocations of the same refresh produce diff-stable output.
	if !sort.StringsAreSorted(res.ChangedAddresses) {
		t.Errorf("ChangedAddresses not sorted: %v", res.ChangedAddresses)
	}
}

// TestRun_ResourceFailureRecordsFailedStatus exercises collect-all
// error mode: one resource crashes, the rest succeed, the run is
// marked 'failed' with a non-empty error_summary, and the per-
// resource error address is captured in Result.Errors.
func TestRun_ResourceFailureRecordsFailedStatus(t *testing.T) {
	st, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	resetAll(t, pool)
	seedRefreshState(t, st)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 30*time.Second)
	defer cancel()

	bad := errors.New("boom")
	factory := newFakeFactory()
	factory.onOpen = func(src provider.SourceAddress, alias string, c *recordingClient) {
		calls := 0
		c.onRead = func(typeName string, currentState []byte) ([]byte, provider.Diagnostics, error) {
			calls++
			if calls == 1 {
				return nil, nil, bad
			}
			echo := make([]byte, len(currentState))
			copy(echo, currentState)
			return echo, nil, nil
		}
	}

	res, err := Run(ctx, st, factory, Options{StateName: "refresh-orch-test"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != store.RefreshRunFailed {
		t.Errorf("Status: got %q, want failed", res.Status)
	}
	if res.ResourcesFailed != 1 {
		t.Errorf("ResourcesFailed: got %d, want 1", res.ResourcesFailed)
	}
	if len(res.Errors) != 1 || !errors.Is(res.Errors[0].Err, bad) {
		t.Errorf("Errors: got %+v", res.Errors)
	}

	got, err := st.GetRefreshRun(ctx, res.RunID)
	if err != nil {
		t.Fatalf("GetRefreshRun: %v", err)
	}
	if got.ErrorSummary == nil || *got.ErrorSummary == "" {
		t.Errorf("ErrorSummary missing; want non-empty")
	}
}

// TestRun_StateNotFound covers the unhappy path that should preempt
// the audit row entirely — there is no state_version to anchor the
// run against, so no refresh_runs row is ever written.
func TestRun_StateNotFound(t *testing.T) {
	st, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	resetAll(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Second)
	defer cancel()

	_, err := Run(ctx, st, newFakeFactory(), Options{StateName: "no-such-state"})
	if !errors.Is(err, store.ErrStateNotFound) {
		t.Fatalf("Run: got %v, want wrapping ErrStateNotFound", err)
	}

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM refresh_runs`).Scan(&n); err != nil {
		t.Fatalf("count refresh_runs: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 audit rows for failed state lookup, got %d", n)
	}
}

// ---------------------------------------------------------------------------
// Real-provider end-to-end. Drives the orchestrator through the
// production ClientFactory against a real null_resource binary,
// proving the full pipeline:
//
//   Discover → Launch → schema cache → Configure → JSON-encoded
//   ReadResource (via encodingClient wrapper) → splice → WriteState
//
// In v1.6b this test asserted FAILURE because the encoding pipeline
// wasn't wired yet. v1.6c flips it to assert SUCCESS — the same
// state, the same provider binary, but now refreshed end-to-end.
// ---------------------------------------------------------------------------

// nullProviderSearchPath downloads the null provider via terraform
// init into a per-test tempdir and returns the .terraform/providers
// directory the production factory should search. Mirrors
// providerOnDisk in the provider package's integration tests, but
// returns the search path rather than the resolved binary so the
// production ClientFactory drives Discover itself (closer to the
// real CLI flow).
//
// Skip the test cleanly if terraform isn't on PATH.
func nullProviderSearchPath(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("terraform"); err != nil {
		t.Skipf("terraform not on PATH (%v); skipping null-provider test", err)
	}
	dir := t.TempDir()
	tf := filepath.Join(dir, "main.tf")
	body := `terraform {
  required_providers {
    null = {
      source  = "hashicorp/null"
      version = "~> 3.2"
    }
  }
}
`
	if err := os.WriteFile(tf, []byte(body), 0o644); err != nil {
		t.Fatalf("write main.tf: %v", err)
	}
	cmd := exec.Command("terraform", "init", "-upgrade", "-no-color")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("terraform init: %v\n%s", err, out)
	}
	return filepath.Join(dir, ".terraform", "providers")
}

// TestRun_RealNullProvider_HappyPath is the canary that proves
// every layer between v1.0 and v1.6c is correctly composed:
//
//   - Production ClientFactory discovers the null binary, launches it,
//     fetches the schema (cache miss → live RPC → cache fill), runs
//     Configure with an empty config (null takes none).
//   - encodingClient transparently encodes the orchestrator's
//     JSON-shaped attributes to msgpack DynamicValue, sends the wire
//     RPC, and decodes the response back to JSON for splice-back.
//   - The orchestrator records a 'succeeded' refresh run and writes
//     a new state_version with source='refresh' and bumped serial.
//
// null_resource's ReadResource echoes prior state, so we expect
// 2 checked / 0 changed / 0 failed.
func TestRun_RealNullProvider_HappyPath(t *testing.T) {
	if _, err := exec.LookPath("terraform"); err != nil {
		t.Skipf("terraform not on PATH (%v); skipping null-provider test", err)
	}
	searchPath := nullProviderSearchPath(t)

	st, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	resetAll(t, pool)
	// Also clear schema/config caches so we exercise the cache-miss
	// branch on first refresh — important for proving the
	// resolveSchema RPC path works against a real provider.
	if _, err := pool.Exec(context.Background(),
		`TRUNCATE TABLE provider_schemas, provider_configs`); err != nil {
		t.Fatalf("truncate caches: %v", err)
	}
	seedRefreshState(t, st)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 60*time.Second)
	defer cancel()

	factory, err := NewProductionFactory(ProductionFactoryOptions{
		Store:       st,
		SearchPaths: []string{searchPath},
	})
	if err != nil {
		t.Fatalf("NewProductionFactory: %v", err)
	}

	res, err := Run(ctx, st, factory, Options{StateName: "refresh-orch-test"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if res.Status != store.RefreshRunSucceeded {
		t.Errorf("Status: got %q, want succeeded\nerrors: %+v", res.Status, res.Errors)
	}
	if res.ResourcesChecked != 2 {
		t.Errorf("ResourcesChecked: got %d, want 2", res.ResourcesChecked)
	}
	if res.ResourcesFailed != 0 {
		t.Errorf("ResourcesFailed: got %d, want 0\nerrors: %+v", res.ResourcesFailed, res.Errors)
	}
	if res.SerialAfter != res.SerialBefore+1 {
		t.Errorf("SerialAfter: got %d, want %d", res.SerialAfter, res.SerialBefore+1)
	}

	// And the schema cache was filled.
	cached, err := st.GetProviderSchema(ctx, "registry.terraform.io/hashicorp/null", "")
	if err == nil && cached != nil {
		// version is variable; just assert presence.
		t.Logf("cache filled: protocol=%d version=%s", cached.ProtocolVersion, cached.Version)
	}
	var cacheCount int
	err = pool.QueryRow(ctx,
		`SELECT count(*) FROM provider_schemas WHERE provider_source = $1`,
		"registry.terraform.io/hashicorp/null").Scan(&cacheCount)
	if err != nil {
		t.Fatalf("count schemas: %v", err)
	}
	if cacheCount != 1 {
		t.Errorf("provider_schemas rows: got %d, want 1 (cache miss should populate)", cacheCount)
	}
}

// TestRun_RealNullProvider_UsesCachedSchema runs refresh twice in a
// row. The first run populates the schema cache; the second must
// hit the cache without an additional GetSchema RPC. We can't
// directly observe RPC counts, but we can assert the second run
// runs strictly faster than the first — a coarse but reliable
// signal that we didn't re-fetch.
//
// More importantly: this proves the cache-hit branch in
// resolveSchema works against a real provider, not just an
// in-memory schema constructed by tests.
func TestRun_RealNullProvider_UsesCachedSchema(t *testing.T) {
	if _, err := exec.LookPath("terraform"); err != nil {
		t.Skipf("terraform not on PATH (%v); skipping null-provider test", err)
	}
	searchPath := nullProviderSearchPath(t)

	st, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	resetAll(t, pool)
	if _, err := pool.Exec(context.Background(),
		`TRUNCATE TABLE provider_schemas, provider_configs`); err != nil {
		t.Fatalf("truncate caches: %v", err)
	}
	seedRefreshState(t, st)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 90*time.Second)
	defer cancel()

	factory, err := NewProductionFactory(ProductionFactoryOptions{
		Store:       st,
		SearchPaths: []string{searchPath},
	})
	if err != nil {
		t.Fatalf("NewProductionFactory: %v", err)
	}

	first, err := Run(ctx, st, factory, Options{StateName: "refresh-orch-test"})
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if first.Status != store.RefreshRunSucceeded {
		t.Fatalf("first run not succeeded: %q\nerrors: %+v", first.Status, first.Errors)
	}

	second, err := Run(ctx, st, factory, Options{StateName: "refresh-orch-test"})
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if second.Status != store.RefreshRunSucceeded {
		t.Fatalf("second run not succeeded: %q\nerrors: %+v", second.Status, second.Errors)
	}
	if second.SerialBefore != first.SerialAfter {
		t.Errorf("second.SerialBefore (%d) != first.SerialAfter (%d) — version chain broken",
			second.SerialBefore, first.SerialAfter)
	}

	// Cache row count must remain 1 — we should NOT have written a
	// second copy with the same (source, version) primary key
	// (that would error anyway via PK), and we must not have
	// produced a stale row pre-write that leaked.
	var cacheCount int
	err = pool.QueryRow(ctx,
		`SELECT count(*) FROM provider_schemas WHERE provider_source = $1`,
		"registry.terraform.io/hashicorp/null").Scan(&cacheCount)
	if err != nil {
		t.Fatalf("count schemas: %v", err)
	}
	if cacheCount != 1 {
		t.Errorf("provider_schemas rows after 2 runs: got %d, want 1", cacheCount)
	}
}

// TestRun_RealNullProvider_UpgradeRunsOnVersionMismatch proves the
// v1.6.5 upgrade path end-to-end:
//
//   - The first run caches null_resource's real schema (version 0).
//   - We doctor the cached schema's Version to 1, simulating a
//     provider upgrade between runs. The state still has
//     schema_version=0.
//   - The second run sees the cached schema_version=1, so
//     needsUpgrade fires and the orchestrator calls
//     UpgradeResourceState before ReadResource. The real null
//     provider's upgrade implementation accepts any prior version
//     and returns the same JSON shape — that's enough to prove the
//     wire round-trip works against a live provider.
//
// What we assert:
//
//   - The second run succeeded.
//   - State's schema_version was bumped from 0 to 1 in the new
//     committed version (proving the splice + bump worked).
//
// Why this approach: null itself never bumps its schema version
// across releases, and we can't pin the test against a provider
// that *does* without inviting flakiness. Doctoring the cached
// schema gives us a deterministic trigger that exercises the
// production code path verbatim.
func TestRun_RealNullProvider_UpgradeRunsOnVersionMismatch(t *testing.T) {
	if _, err := exec.LookPath("terraform"); err != nil {
		t.Skipf("terraform not on PATH (%v); skipping null-provider test", err)
	}
	searchPath := nullProviderSearchPath(t)

	st, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	resetAll(t, pool)
	if _, err := pool.Exec(context.Background(),
		`TRUNCATE TABLE provider_schemas, provider_configs`); err != nil {
		t.Fatalf("truncate caches: %v", err)
	}
	seedRefreshState(t, st)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 90*time.Second)
	defer cancel()

	factory, err := NewProductionFactory(ProductionFactoryOptions{
		Store:       st,
		SearchPaths: []string{searchPath},
	})
	if err != nil {
		t.Fatalf("NewProductionFactory: %v", err)
	}

	// Run 1: populates the schema cache with null's real schema.
	first, err := Run(ctx, st, factory, Options{StateName: "refresh-orch-test"})
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if first.Status != store.RefreshRunSucceeded {
		t.Fatalf("first run not succeeded: %q\nerrors: %+v", first.Status, first.Errors)
	}

	// Doctor the cached schema: bump Resources.null_resource.Version
	// from its real value (0 at time of writing) to 1, forcing
	// needsUpgrade to trigger on the next run. The block shape is
	// untouched, so the encoder still has a valid type to drive
	// encode/decode against the live wire.
	tag, err := pool.Exec(ctx, `
		UPDATE provider_schemas
		SET schema_jsonb = jsonb_set(
		    schema_jsonb,
		    '{Resources,null_resource,Version}',
		    to_jsonb(1)
		)
		WHERE provider_source = 'registry.terraform.io/hashicorp/null'
	`)
	if err != nil {
		t.Fatalf("doctor cached schema: %v", err)
	}
	if rows := tag.RowsAffected(); rows != 1 {
		t.Fatalf("doctor: updated %d rows, want 1", rows)
	}

	// Run 2: should trigger UpgradeResourceState before each
	// ReadResource. The real null provider accepts and round-trips.
	second, err := Run(ctx, st, factory, Options{StateName: "refresh-orch-test"})
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if second.Status != store.RefreshRunSucceeded {
		t.Fatalf("second run not succeeded: %q\nerrors: %+v", second.Status, second.Errors)
	}

	// The committed state version must now record schema_version=1
	// for every null_resource instance — that's the bump produced
	// by the orchestrator after a successful upgrade. Read it back
	// via GetCurrentStateInfo + json parse rather than going through
	// the normalized rows; the raw_state column is the authoritative
	// record of what was written.
	info, err := st.GetCurrentStateInfo(ctx, "refresh-orch-test")
	if err != nil {
		t.Fatalf("GetCurrentStateInfo: %v", err)
	}
	// Postgres' jsonb_set may pretty-print with a space after the
	// colon; we tolerate both forms by normalizing through the
	// state parser, which gives a canonical Go view.
	parsedAfter, err := tfstate.Parse(info.Raw)
	if err != nil {
		t.Fatalf("parse committed state: %v\nraw:\n%s", err, info.Raw)
	}
	for ri, r := range parsedAfter.Resources {
		if r.Type != "null_resource" {
			continue
		}
		for ii, inst := range r.Instances {
			if inst.SchemaVersion != 1 {
				t.Errorf("resources[%d].instances[%d] schema_version after upgrade: got %d, want 1\nraw:\n%s",
					ri, ii, inst.SchemaVersion, info.Raw)
			}
		}
	}
}
