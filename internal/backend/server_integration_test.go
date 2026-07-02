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

func TestStateEngineResolveExpandAndSlice(t *testing.T) {
	pool := requireDB(t)
	t.Cleanup(pool.Close)
	resetTables(t, pool)
	setSelfHostedTenantLifecycleStatus(t, pool, store.LifecycleStatusActive)

	srv := New(store.New(pool.Pool), slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := srv.Handler()

	state := "engine-scope"
	body := stateWithGraph(3)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/states/"+state, bytes.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("seed POST status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}

	resolveReq := httptest.NewRequest(http.MethodPost, "/v1/state-engine/state/resolve",
		strings.NewReader(`{"state":"engine-scope"}`))
	resolveReq.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, resolveReq)
	if w.Code != http.StatusOK {
		t.Fatalf("state resolve status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	var resolved struct {
		State   string `json:"state"`
		StateID string `json:"state_id"`
		Lineage string `json:"lineage"`
		Serial  int64  `json:"serial"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resolved); err != nil {
		t.Fatalf("decode resolve response: %v", err)
	}
	if resolved.State != state {
		t.Fatalf("resolved state = %q, want %q", resolved.State, state)
	}
	if resolved.StateID == "" || resolved.Lineage == "" || resolved.Serial != 1 {
		t.Fatalf("unexpected resolved metadata: %+v", resolved)
	}

	expandReq := httptest.NewRequest(http.MethodPost, "/v1/state-engine/scope/expand", strings.NewReader(`{
	  "state":"engine-scope",
	  "selectors":[{"kind":"resource_address","value":"aws_subnet.private"}],
	  "client_context":{
	    "explicit_write_candidates":["aws_subnet.private"],
	    "explicit_read_candidates":[],
	    "undeployed_config_candidates":[]
	  }
	}`))
	expandReq.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, expandReq)
	if w.Code != http.StatusOK {
		t.Fatalf("scope expand status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	var expanded struct {
		ContractSource string   `json:"contract_source"`
		FetchAddresses []string `json:"fetch_addresses"`
		ScopeContract  struct {
			FetchAddresses      []string `json:"fetch_addresses"`
			WriteAddresses      []string `json:"write_addresses"`
			ReadAddresses       []string `json:"read_addresses"`
			ConfigRequiredNodes []string `json:"config_required_nodes"`
			RemovedConfigNodes  []string `json:"removed_config_nodes"`
			Missing             []string `json:"missing_from_state"`
			Undeployed          []string `json:"undeployed_candidates"`
			UnknownMissing      []string `json:"unknown_missing_from_state"`
			Confidence          string   `json:"confidence"`
			Notes               []string `json:"notes"`
			Diagnostics         struct {
				GraphCacheHit         bool `json:"graph_cache_hit"`
				RealizedResourceCount int  `json:"realized_resource_count"`
				DependencyEdgeCount   int  `json:"dependency_edge_count"`
				InventoryScanCount    int  `json:"inventory_scan_count"`
			} `json:"diagnostics"`
		} `json:"scope_contract"`
		WriteClosure   []string         `json:"realized_write_closure"`
		ReadClosure    []string         `json:"realized_read_closure"`
		Missing        []string         `json:"missing_from_state"`
		Undeployed     []string         `json:"undeployed_candidates"`
		UnknownMissing []string         `json:"unknown_missing_from_state"`
		Confidence     string           `json:"confidence"`
		Reservations   []map[string]any `json:"reservation_candidates"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &expanded); err != nil {
		t.Fatalf("decode expand response: %v", err)
	}
	if !reflect.DeepEqual(expanded.WriteClosure, []string{"aws_subnet.private"}) {
		t.Fatalf("write closure = %v, want [aws_subnet.private]", expanded.WriteClosure)
	}
	if !reflect.DeepEqual(expanded.ReadClosure, []string{"aws_vpc.main"}) {
		t.Fatalf("read closure = %v, want [aws_vpc.main]", expanded.ReadClosure)
	}
	if expanded.ContractSource != "backend_authoritative" {
		t.Fatalf("contract_source = %q, want backend_authoritative", expanded.ContractSource)
	}
	if !reflect.DeepEqual(expanded.FetchAddresses, []string{"aws_subnet.private", "aws_vpc.main"}) {
		t.Fatalf("fetch_addresses = %v, want [aws_subnet.private aws_vpc.main]", expanded.FetchAddresses)
	}
	if !reflect.DeepEqual(expanded.ScopeContract.FetchAddresses, []string{"aws_subnet.private", "aws_vpc.main"}) {
		t.Fatalf("scope_contract.fetch_addresses = %v, want [aws_subnet.private aws_vpc.main]", expanded.ScopeContract.FetchAddresses)
	}
	if !reflect.DeepEqual(expanded.ScopeContract.WriteAddresses, []string{"aws_subnet.private"}) {
		t.Fatalf("scope_contract.write_addresses = %v, want [aws_subnet.private]", expanded.ScopeContract.WriteAddresses)
	}
	if !reflect.DeepEqual(expanded.ScopeContract.ReadAddresses, []string{"aws_vpc.main"}) {
		t.Fatalf("scope_contract.read_addresses = %v, want [aws_vpc.main]", expanded.ScopeContract.ReadAddresses)
	}
	if len(expanded.Missing) != 0 {
		t.Fatalf("missing_from_state = %v, want empty", expanded.Missing)
	}
	if len(expanded.Undeployed) != 0 {
		t.Fatalf("undeployed_candidates = %v, want empty", expanded.Undeployed)
	}
	if len(expanded.UnknownMissing) != 0 {
		t.Fatalf("unknown_missing_from_state = %v, want empty", expanded.UnknownMissing)
	}
	if expanded.Confidence != "safe" {
		t.Fatalf("confidence = %q, want safe", expanded.Confidence)
	}
	if expanded.ScopeContract.Diagnostics.RealizedResourceCount != 2 {
		t.Fatalf("realized_resource_count = %d, want 2", expanded.ScopeContract.Diagnostics.RealizedResourceCount)
	}
	if expanded.ScopeContract.Diagnostics.DependencyEdgeCount != 1 {
		t.Fatalf("dependency_edge_count = %d, want 1", expanded.ScopeContract.Diagnostics.DependencyEdgeCount)
	}
	if expanded.ScopeContract.Diagnostics.InventoryScanCount != 0 {
		t.Fatalf("inventory_scan_count = %d, want 0 for direct resource selector", expanded.ScopeContract.Diagnostics.InventoryScanCount)
	}
	if len(expanded.Reservations) != 2 {
		t.Fatalf("reservations = %v, want 2 entries", expanded.Reservations)
	}

	sliceReq := httptest.NewRequest(http.MethodPost, "/v1/state-engine/state/slice", strings.NewReader(`{
	  "state":"engine-scope",
	  "addresses":["aws_subnet.private","aws_vpc.main"],
	  "base_serial":3
	}`))
	sliceReq.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, sliceReq)
	if w.Code != http.StatusOK {
		t.Fatalf("state slice status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	var sliced struct {
		State string `json:"state"`
		Slice struct {
			Resources []store.StateEngineResource `json:"resources"`
		} `json:"slice"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &sliced); err != nil {
		t.Fatalf("decode slice response: %v", err)
	}
	if sliced.State != state {
		t.Fatalf("sliced state = %q, want %q", sliced.State, state)
	}
	if len(sliced.Slice.Resources) != 2 {
		t.Fatalf("slice resources = %d, want 2", len(sliced.Slice.Resources))
	}
	if sliced.Slice.Resources[0].AttributesHash == "" && sliced.Slice.Resources[1].AttributesHash == "" {
		t.Fatalf("slice attributes_hash missing on both resources: %+v", sliced.Slice.Resources)
	}

	expandReq = httptest.NewRequest(http.MethodPost, "/v1/state-engine/scope/expand", strings.NewReader(`{
	  "state":"engine-scope",
	  "selectors":[{"kind":"resource_address","value":"aws_subnet.future"}],
	  "client_context":{
	    "explicit_write_candidates":["aws_subnet.future"],
	    "explicit_read_candidates":[],
	    "undeployed_config_candidates":["aws_subnet.future"],
	    "config_nodes":[
	      {"address":"aws_subnet.future","dependencies":["aws_vpc.main"]}
	    ]
	  }
	}`))
	expandReq.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, expandReq)
	if w.Code != http.StatusOK {
		t.Fatalf("scope expand undeployed status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &expanded); err != nil {
		t.Fatalf("decode undeployed expand response: %v", err)
	}
	if !reflect.DeepEqual(expanded.ReadClosure, []string{"aws_vpc.main"}) {
		t.Fatalf("read closure for undeployed = %v, want [aws_vpc.main]", expanded.ReadClosure)
	}
	if !reflect.DeepEqual(expanded.ScopeContract.FetchAddresses, []string{"aws_vpc.main"}) {
		t.Fatalf("scope_contract.fetch_addresses for undeployed = %v, want [aws_vpc.main]", expanded.ScopeContract.FetchAddresses)
	}
	if !reflect.DeepEqual(expanded.ScopeContract.ConfigRequiredNodes, []string{"aws_subnet.future"}) {
		t.Fatalf("scope_contract.config_required_nodes = %v, want [aws_subnet.future]", expanded.ScopeContract.ConfigRequiredNodes)
	}
	if !reflect.DeepEqual(expanded.Missing, []string{"aws_subnet.future"}) {
		t.Fatalf("missing_from_state = %v, want [aws_subnet.future]", expanded.Missing)
	}
	if !reflect.DeepEqual(expanded.ScopeContract.Missing, []string{"aws_subnet.future"}) {
		t.Fatalf("scope_contract.missing_from_state = %v, want [aws_subnet.future]", expanded.ScopeContract.Missing)
	}
	if !reflect.DeepEqual(expanded.Undeployed, []string{"aws_subnet.future"}) {
		t.Fatalf("undeployed_candidates = %v, want [aws_subnet.future]", expanded.Undeployed)
	}
	if !reflect.DeepEqual(expanded.ScopeContract.Undeployed, []string{"aws_subnet.future"}) {
		t.Fatalf("scope_contract.undeployed_candidates = %v, want [aws_subnet.future]", expanded.ScopeContract.Undeployed)
	}
	if len(expanded.UnknownMissing) != 0 {
		t.Fatalf("unknown_missing_from_state = %v, want empty", expanded.UnknownMissing)
	}
	if len(expanded.ScopeContract.UnknownMissing) != 0 {
		t.Fatalf("scope_contract.unknown_missing_from_state = %v, want empty", expanded.ScopeContract.UnknownMissing)
	}
	if expanded.Confidence != "safe" {
		t.Fatalf("confidence for undeployed = %q, want safe", expanded.Confidence)
	}
	if expanded.ScopeContract.Confidence != "safe" {
		t.Fatalf("scope_contract.confidence for undeployed = %q, want safe", expanded.ScopeContract.Confidence)
	}

	expandReq = httptest.NewRequest(http.MethodPost, "/v1/state-engine/scope/expand", strings.NewReader(`{
	  "state":"engine-scope",
	  "selectors":[{"kind":"resource_address","value":"aws_subnet.future"}],
	  "client_context":{
	    "explicit_write_candidates":["aws_subnet.future"],
	    "explicit_read_candidates":["aws_vpc.main"],
	    "undeployed_config_candidates":["aws_subnet.future","aws_internet_gateway.future"],
	    "config_nodes":[
	      {"address":"aws_subnet.future","dependencies":["aws_vpc.main"]},
	      {"address":"aws_vpc.main","dependencies":["aws_internet_gateway.future"]},
	      {"address":"aws_internet_gateway.future","dependencies":[]}
	    ]
	  }
	}`))
	expandReq.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, expandReq)
	if w.Code != http.StatusOK {
		t.Fatalf("scope expand realized-read->undeployed status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &expanded); err != nil {
		t.Fatalf("decode realized-read->undeployed expand response: %v", err)
	}
	if !reflect.DeepEqual(expanded.ReadClosure, []string{"aws_vpc.main"}) {
		t.Fatalf("read closure for realized-read->undeployed = %v, want [aws_vpc.main]", expanded.ReadClosure)
	}
	if !reflect.DeepEqual(expanded.ScopeContract.FetchAddresses, []string{"aws_vpc.main"}) {
		t.Fatalf("scope_contract.fetch_addresses for realized-read->undeployed = %v, want [aws_vpc.main]", expanded.ScopeContract.FetchAddresses)
	}
	if !reflect.DeepEqual(expanded.ScopeContract.ConfigRequiredNodes, []string{"aws_internet_gateway.future", "aws_subnet.future"}) {
		t.Fatalf("scope_contract.config_required_nodes for realized-read->undeployed = %v, want [aws_internet_gateway.future aws_subnet.future]", expanded.ScopeContract.ConfigRequiredNodes)
	}
	if !reflect.DeepEqual(expanded.ScopeContract.Undeployed, []string{"aws_internet_gateway.future", "aws_subnet.future"}) {
		t.Fatalf("scope_contract.undeployed_candidates for realized-read->undeployed = %v, want [aws_internet_gateway.future aws_subnet.future]", expanded.ScopeContract.Undeployed)
	}
	if len(expanded.ScopeContract.UnknownMissing) != 0 {
		t.Fatalf("scope_contract.unknown_missing_from_state for realized-read->undeployed = %v, want empty", expanded.ScopeContract.UnknownMissing)
	}
	if expanded.ScopeContract.Confidence != "safe" {
		t.Fatalf("scope_contract.confidence for realized-read->undeployed = %q, want safe", expanded.ScopeContract.Confidence)
	}
	if len(expanded.ScopeContract.Notes) == 0 {
		t.Fatalf("scope_contract.notes for realized-read->undeployed should not be empty")
	}

	expandReq = httptest.NewRequest(http.MethodPost, "/v1/state-engine/scope/expand", strings.NewReader(`{
	  "state":"engine-scope",
	  "selectors":[{"kind":"resource_address","value":"aws_subnet.private"}],
	  "client_context":{
	    "explicit_write_candidates":["aws_subnet.private"],
	    "explicit_read_candidates":[],
	    "undeployed_config_candidates":[],
	    "removed_config_candidates":["aws_subnet.private"]
	  }
	}`))
	expandReq.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, expandReq)
	if w.Code != http.StatusOK {
		t.Fatalf("scope expand removed-realized status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &expanded); err != nil {
		t.Fatalf("decode removed-realized expand response: %v", err)
	}
	if !reflect.DeepEqual(expanded.ScopeContract.RemovedConfigNodes, []string{"aws_subnet.private"}) {
		t.Fatalf("scope_contract.removed_config_nodes = %v, want [aws_subnet.private]", expanded.ScopeContract.RemovedConfigNodes)
	}
	if !reflect.DeepEqual(expanded.ScopeContract.WriteAddresses, []string{"aws_subnet.private"}) {
		t.Fatalf("scope_contract.write_addresses for removed-realized = %v, want [aws_subnet.private]", expanded.ScopeContract.WriteAddresses)
	}
	if expanded.ScopeContract.Confidence != "safe" {
		t.Fatalf("scope_contract.confidence for removed-realized = %q, want safe", expanded.ScopeContract.Confidence)
	}
	if len(expanded.ScopeContract.Notes) == 0 {
		t.Fatalf("scope_contract.notes for removed-realized should not be empty")
	}

	expandReq = httptest.NewRequest(http.MethodPost, "/v1/state-engine/scope/expand", strings.NewReader(`{
	  "state":"engine-scope",
	  "selectors":[{"kind":"resource_address","value":"aws_subnet.deleted_already"}],
	  "client_context":{
	    "explicit_write_candidates":["aws_subnet.deleted_already"],
	    "explicit_read_candidates":[],
	    "undeployed_config_candidates":[],
	    "removed_config_candidates":["aws_subnet.deleted_already"]
	  }
	}`))
	expandReq.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, expandReq)
	if w.Code != http.StatusOK {
		t.Fatalf("scope expand removed-absent status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &expanded); err != nil {
		t.Fatalf("decode removed-absent expand response: %v", err)
	}
	if !reflect.DeepEqual(expanded.ScopeContract.RemovedConfigNodes, []string{"aws_subnet.deleted_already"}) {
		t.Fatalf("scope_contract.removed_config_nodes absent = %v, want [aws_subnet.deleted_already]", expanded.ScopeContract.RemovedConfigNodes)
	}
	if len(expanded.ScopeContract.UnknownMissing) != 0 {
		t.Fatalf("scope_contract.unknown_missing_from_state for removed-absent = %v, want empty", expanded.ScopeContract.UnknownMissing)
	}
	if expanded.ScopeContract.Confidence != "safe" {
		t.Fatalf("scope_contract.confidence for removed-absent = %q, want safe", expanded.ScopeContract.Confidence)
	}
	if len(expanded.ScopeContract.Notes) == 0 {
		t.Fatalf("scope_contract.notes for removed-absent should not be empty")
	}

	sliceReq = httptest.NewRequest(http.MethodPost, "/v1/state-engine/state/slice", strings.NewReader(`{
	  "state":"engine-scope",
	  "addresses":[],
	  "base_serial":3
	}`))
	sliceReq.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, sliceReq)
	if w.Code != http.StatusOK {
		t.Fatalf("empty state slice status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &sliced); err != nil {
		t.Fatalf("decode empty slice response: %v", err)
	}
	if len(sliced.Slice.Resources) != 0 {
		t.Fatalf("empty slice resources = %d, want 0", len(sliced.Slice.Resources))
	}
}

func TestStateEngineCoarseLockBlocksTerraformLockAndForceUnlock(t *testing.T) {
	pool := requireDB(t)
	t.Cleanup(pool.Close)
	resetTables(t, pool)
	setSelfHostedTenantLifecycleStatus(t, pool, store.LifecycleStatusActive)

	srv := New(store.New(pool.Pool), slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := srv.Handler()

	state := "engine-lock"

	acquireReq := httptest.NewRequest(http.MethodPost, "/v1/state-engine/terraform-lock/acquire", strings.NewReader(`{
	  "state":"engine-lock",
	  "apply_id":"apply-1",
	  "holder":"alice@example.com@kl",
	  "scope_summary":["null_resource.example"]
	}`))
	acquireReq.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, acquireReq)
	if w.Code != http.StatusOK {
		t.Fatalf("coarse lock acquire status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}

	lockBody := `{"ID":"tf-lock-1","Operation":"OperationTypeApply","Info":"","Who":"bob@laptop","Version":"1.13.4","Created":"2026-06-24T10:00:00Z","Path":"http://localhost:8080/v1/states/engine-lock"}`
	lockReq := httptest.NewRequest("LOCK", "/v1/states/"+state, strings.NewReader(lockBody))
	lockReq.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, lockReq)
	if w.Code != http.StatusLocked {
		t.Fatalf("terraform LOCK during coarse lock status = %d, want 423 (body=%s)", w.Code, w.Body.String())
	}
	var existing store.LockInfo
	if err := json.Unmarshal(w.Body.Bytes(), &existing); err != nil {
		t.Fatalf("decode conflicting lock: %v", err)
	}
	if !strings.HasPrefix(existing.Path, "state-engine://") {
		t.Fatalf("conflicting lock path = %q, want state-engine://...", existing.Path)
	}

	forceReq := httptest.NewRequest("UNLOCK", "/v1/states/"+state, nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, forceReq)
	if w.Code != http.StatusOK {
		t.Fatalf("force unlock status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}

	lockReq = httptest.NewRequest("LOCK", "/v1/states/"+state, strings.NewReader(lockBody))
	lockReq.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, lockReq)
	if w.Code != http.StatusLocked {
		t.Fatalf("terraform LOCK after force-unlock attempt status = %d, want 423 (body=%s)", w.Code, w.Body.String())
	}

	releaseReq := httptest.NewRequest(http.MethodPost, "/v1/state-engine/terraform-lock/release", strings.NewReader(`{
	  "state":"engine-lock",
	  "apply_id":"apply-1",
	  "actor":"alice@example.com@kl"
	}`))
	releaseReq.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, releaseReq)
	if w.Code != http.StatusOK {
		t.Fatalf("coarse lock release status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}

	lockReq = httptest.NewRequest("LOCK", "/v1/states/"+state, strings.NewReader(lockBody))
	lockReq.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, lockReq)
	if w.Code != http.StatusOK {
		t.Fatalf("terraform LOCK after coarse lock release status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
}

func TestAdminRollbackApplyBlockedByStateEngineCoarseLock(t *testing.T) {
	pool := requireDB(t)
	t.Cleanup(pool.Close)
	resetTables(t, pool)
	setSelfHostedTenantLifecycleStatus(t, pool, store.LifecycleStatusActive)

	srv := New(store.New(pool.Pool), slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := srv.Handler()

	state := "rollback-locked"

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/states/"+state, bytes.NewReader(minimalState(1))))
	if w.Code != http.StatusOK {
		t.Fatalf("seed POST serial=1 status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/states/"+state, bytes.NewReader(minimalState(2))))
	if w.Code != http.StatusOK {
		t.Fatalf("seed POST serial=2 status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}

	acquireReq := httptest.NewRequest(http.MethodPost, "/v1/state-engine/terraform-lock/acquire", strings.NewReader(`{
	  "state":"rollback-locked",
	  "apply_id":"apply-rollback-lock",
	  "holder":"alice@example.com@kl",
	  "scope_summary":["full-state rollback"]
	}`))
	acquireReq.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, acquireReq)
	if w.Code != http.StatusOK {
		t.Fatalf("coarse lock acquire status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}

	rollbackReq := httptest.NewRequest(http.MethodPost, "/v1/admin/state/rollback/apply?name="+state, strings.NewReader(`{
	  "to":"1",
	  "actor":"rollback-tester"
	}`))
	rollbackReq.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, rollbackReq)
	if w.Code != http.StatusLocked {
		t.Fatalf("admin rollback apply status = %d, want 423 (body=%s)", w.Code, w.Body.String())
	}
	var resp struct {
		Error string         `json:"error"`
		Lock  store.LockInfo `json:"lock"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode rollback lock response: %v", err)
	}
	if resp.Error != "state locked" {
		t.Fatalf("rollback lock error = %q, want %q", resp.Error, "state locked")
	}
	if !strings.HasPrefix(resp.Lock.Path, "state-engine://") {
		t.Fatalf("rollback lock path = %q, want state-engine://...", resp.Lock.Path)
	}
}

func TestStateEngineNativeResourceRemoveAndMove(t *testing.T) {
	pool := requireDB(t)
	t.Cleanup(pool.Close)
	resetTables(t, pool)
	setSelfHostedTenantLifecycleStatus(t, pool, store.LifecycleStatusActive)

	srv := New(store.New(pool.Pool), slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := srv.Handler()

	state := "engine-native-mutate"
	body := stateWithGraph(3)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/states/"+state, bytes.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("seed POST status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}

	previewRemoveReq := httptest.NewRequest(http.MethodPost, "/v1/state-engine/resource-remove/preview", strings.NewReader(`{
	  "state":"engine-native-mutate",
	  "address":"aws_subnet.private[0]"
	}`))
	previewRemoveReq.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, previewRemoveReq)
	if w.Code != http.StatusOK {
		t.Fatalf("remove preview status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	var removePreview struct {
		Preview store.ResourceMutationPreview `json:"preview"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &removePreview); err != nil {
		t.Fatalf("decode remove preview: %v", err)
	}
	if removePreview.Preview.Action != "remove" {
		t.Fatalf("remove preview action = %q, want remove", removePreview.Preview.Action)
	}
	if len(removePreview.Preview.Dependencies) != 1 || removePreview.Preview.Dependencies[0] != "aws_vpc.main" {
		t.Fatalf("remove preview dependencies = %v", removePreview.Preview.Dependencies)
	}

	applyMoveReq := httptest.NewRequest(http.MethodPost, "/v1/state-engine/resource-move/apply", strings.NewReader(`{
	  "state":"engine-native-mutate",
	  "address":"aws_vpc.main",
	  "to":"module.edge.aws_vpc.main",
	  "actor":"tester"
	}`))
	applyMoveReq.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, applyMoveReq)
	if w.Code != http.StatusOK {
		t.Fatalf("move apply status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}

	sliceReq := httptest.NewRequest(http.MethodPost, "/v1/state-engine/state/slice", strings.NewReader(`{
	  "state":"engine-native-mutate",
	  "addresses":["module.edge.aws_vpc.main","aws_subnet.private[0]"]
	}`))
	sliceReq.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, sliceReq)
	if w.Code != http.StatusOK {
		t.Fatalf("slice after move status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	var sliced struct {
		Slice struct {
			Resources []store.StateEngineResource `json:"resources"`
		} `json:"slice"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &sliced); err != nil {
		t.Fatalf("decode slice after move: %v", err)
	}
	if len(sliced.Slice.Resources) != 2 {
		t.Fatalf("slice resources after move = %d, want 2", len(sliced.Slice.Resources))
	}
	foundMoved := false
	foundDependencyRewrite := false
	for _, resource := range sliced.Slice.Resources {
		if resource.Address == "module.edge.aws_vpc.main" {
			foundMoved = true
		}
		if resource.Address == "aws_subnet.private[0]" && len(resource.Dependencies) == 1 && resource.Dependencies[0] == "module.edge.aws_vpc.main" {
			foundDependencyRewrite = true
		}
	}
	if !foundMoved {
		t.Fatalf("moved address not present in slice: %+v", sliced.Slice.Resources)
	}
	if !foundDependencyRewrite {
		t.Fatalf("dependency rewrite not reflected in slice: %+v", sliced.Slice.Resources)
	}

	applyRemoveReq := httptest.NewRequest(http.MethodPost, "/v1/state-engine/resource-remove/apply", strings.NewReader(`{
	  "state":"engine-native-mutate",
	  "address":"aws_subnet.private[0]",
	  "actor":"tester"
	}`))
	applyRemoveReq.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, applyRemoveReq)
	if w.Code != http.StatusOK {
		t.Fatalf("remove apply status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}

	sliceReq = httptest.NewRequest(http.MethodPost, "/v1/state-engine/state/slice", strings.NewReader(`{
	  "state":"engine-native-mutate",
	  "addresses":["module.edge.aws_vpc.main","aws_subnet.private[0]"]
	}`))
	sliceReq.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, sliceReq)
	if w.Code != http.StatusOK {
		t.Fatalf("slice after remove status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &sliced); err != nil {
		t.Fatalf("decode slice after remove: %v", err)
	}
	if len(sliced.Slice.Resources) != 1 || sliced.Slice.Resources[0].Address != "module.edge.aws_vpc.main" {
		t.Fatalf("slice after remove = %+v, want only moved vpc", sliced.Slice.Resources)
	}

	previewRollbackReq := httptest.NewRequest(http.MethodPost, "/v1/state-engine/resource-rollback/preview", strings.NewReader(`{
	  "state":"engine-native-mutate",
	  "address":"aws_subnet.private[0]",
	  "to":"@1"
	}`))
	previewRollbackReq.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, previewRollbackReq)
	if w.Code != http.StatusOK {
		t.Fatalf("rollback preview status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	var rollbackPreview struct {
		Preview store.ResourceRollbackPreview `json:"preview"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &rollbackPreview); err != nil {
		t.Fatalf("decode rollback preview: %v", err)
	}
	if rollbackPreview.Preview.Action != "restore" {
		t.Fatalf("rollback preview action = %q, want restore", rollbackPreview.Preview.Action)
	}

	applyRollbackReq := httptest.NewRequest(http.MethodPost, "/v1/state-engine/resource-rollback/apply", strings.NewReader(`{
	  "state":"engine-native-mutate",
	  "address":"aws_subnet.private[0]",
	  "to":"@1",
	  "actor":"tester"
	}`))
	applyRollbackReq.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, applyRollbackReq)
	if w.Code != http.StatusOK {
		t.Fatalf("rollback apply status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}

	sliceReq = httptest.NewRequest(http.MethodPost, "/v1/state-engine/state/slice", strings.NewReader(`{
	  "state":"engine-native-mutate",
	  "addresses":["module.edge.aws_vpc.main","aws_subnet.private[0]"]
	}`))
	sliceReq.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, sliceReq)
	if w.Code != http.StatusOK {
		t.Fatalf("slice after rollback status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &sliced); err != nil {
		t.Fatalf("decode slice after rollback: %v", err)
	}
	if len(sliced.Slice.Resources) != 2 {
		t.Fatalf("slice resources after rollback = %d, want 2", len(sliced.Slice.Resources))
	}
}

func TestStateEngineSnapshotCommit(t *testing.T) {
	pool := requireDB(t)
	t.Cleanup(pool.Close)
	resetTables(t, pool)
	setSelfHostedTenantLifecycleStatus(t, pool, store.LifecycleStatusActive)

	srv := New(store.New(pool.Pool), slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := srv.Handler()

	state := "engine-snapshot-commit"
	body := stateWithGraph(1)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/states/"+state, bytes.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("seed POST status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}

	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode seed state: %v", err)
	}
	resources, _ := decoded["resources"].([]any)
	for _, item := range resources {
		resource, _ := item.(map[string]any)
		if resource["type"] != "aws_vpc" || resource["name"] != "main" {
			continue
		}
		instances, _ := resource["instances"].([]any)
		if len(instances) == 0 {
			continue
		}
		instance, _ := instances[0].(map[string]any)
		attrs, _ := instance["attributes"].(map[string]any)
		attrs["id"] = "vpc-999"
	}
	decoded["serial"] = float64(2)
	updatedBody, err := json.Marshal(decoded)
	if err != nil {
		t.Fatalf("marshal updated state: %v", err)
	}

	commitReq := httptest.NewRequest(http.MethodPost, "/v1/state-engine/state/commit", strings.NewReader(fmt.Sprintf(`{
	  "state":"%s",
	  "apply_id":"apply-commit-1",
	  "base_serial":1,
	  "mode":"snapshot",
	  "raw_state":%q,
	  "write_set":["aws_vpc.main"],
	  "source":"state-engine-apply",
	  "actor":"tester"
	}`, state, string(updatedBody))))
	commitReq.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, commitReq)
	if w.Code != http.StatusOK {
		t.Fatalf("snapshot commit status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	var commitResp struct {
		CommittedSerial int64  `json:"committed_serial"`
		CommitMode      string `json:"commit_mode"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &commitResp); err != nil {
		t.Fatalf("decode snapshot commit: %v", err)
	}
	if commitResp.CommittedSerial != 2 {
		t.Fatalf("committed serial = %d, want 2", commitResp.CommittedSerial)
	}
	if commitResp.CommitMode != "snapshot-selected" {
		t.Fatalf("commit mode = %q, want snapshot-selected", commitResp.CommitMode)
	}

	decoded["serial"] = float64(3)
	updatedBody2, err := json.Marshal(decoded)
	if err != nil {
		t.Fatalf("marshal updated delta state: %v", err)
	}
	var deltaState struct {
		TerraformVersion string           `json:"terraform_version"`
		Lineage          string           `json:"lineage"`
		Outputs          map[string]any   `json:"outputs"`
		CheckResults     json.RawMessage  `json:"check_results"`
		Resources        []map[string]any `json:"resources"`
	}
	if err := json.Unmarshal(updatedBody2, &deltaState); err != nil {
		t.Fatalf("decode updated delta state: %v", err)
	}
	var selected []map[string]any
	for _, resource := range deltaState.Resources {
		if resource["type"] == "aws_vpc" && resource["name"] == "main" {
			selected = append(selected, resource)
		}
	}
	deltaBody, err := json.Marshal(map[string]any{
		"terraform_version": deltaState.TerraformVersion,
		"lineage":           deltaState.Lineage,
		"outputs":           deltaState.Outputs,
		"check_results":     deltaState.CheckResults,
		"resources":         selected,
		"write_set":         []string{"aws_vpc.main"},
	})
	if err != nil {
		t.Fatalf("marshal delta commit body: %v", err)
	}
	commitReq = httptest.NewRequest(http.MethodPost, "/v1/state-engine/state/commit", strings.NewReader(fmt.Sprintf(`{
	  "state":"%s",
	  "apply_id":"apply-commit-2",
	  "base_serial":2,
	  "mode":"delta",
	  "delta":%s,
	  "source":"state-engine-apply",
	  "actor":"tester"
	}`, state, string(deltaBody))))
	commitReq.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, commitReq)
	if w.Code != http.StatusOK {
		t.Fatalf("delta commit status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &commitResp); err != nil {
		t.Fatalf("decode delta commit: %v", err)
	}
	if commitResp.CommittedSerial != 3 {
		t.Fatalf("delta committed serial = %d, want 3", commitResp.CommittedSerial)
	}
	if commitResp.CommitMode != "delta" {
		t.Fatalf("delta commit mode = %q, want delta", commitResp.CommitMode)
	}

	sliceReq := httptest.NewRequest(http.MethodPost, "/v1/state-engine/state/slice", strings.NewReader(fmt.Sprintf(`{
	  "state":"%s",
	  "addresses":["aws_vpc.main","aws_subnet.private[0]"]
	}`, state)))
	sliceReq.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, sliceReq)
	if w.Code != http.StatusOK {
		t.Fatalf("slice after snapshot commit status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	var sliced struct {
		Slice struct {
			Resources []store.StateEngineResource `json:"resources"`
		} `json:"slice"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &sliced); err != nil {
		t.Fatalf("decode slice after snapshot commit: %v", err)
	}
	if len(sliced.Slice.Resources) != 2 {
		t.Fatalf("slice resources after snapshot commit = %d, want 2", len(sliced.Slice.Resources))
	}
	foundUpdatedVPC := false
	for _, resource := range sliced.Slice.Resources {
		if resource.Address == "aws_vpc.main" && strings.Contains(string(resource.Attributes), "vpc-999") {
			foundUpdatedVPC = true
		}
	}
	if !foundUpdatedVPC {
		t.Fatalf("updated aws_vpc.main not reflected in slice: %+v", sliced.Slice.Resources)
	}
}

func TestStateEngineApplyRunLifecycle(t *testing.T) {
	pool := requireDB(t)
	t.Cleanup(pool.Close)
	resetTables(t, pool)
	setSelfHostedTenantLifecycleStatus(t, pool, store.LifecycleStatusActive)

	srv := New(store.New(pool.Pool), slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := srv.Handler()

	state := "engine-apply-run"
	body := minimalState(1)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/states/"+state, bytes.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("seed POST status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}

	beginReq := httptest.NewRequest(http.MethodPost, "/v1/state-engine/apply-runs/begin", strings.NewReader(fmt.Sprintf(`{
	  "state":"%s",
	  "actor":"tester",
	  "source_serial":1
	}`, state)))
	beginReq.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, beginReq)
	if w.Code != http.StatusOK {
		t.Fatalf("apply-run begin status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	var run store.ApplyRun
	if err := json.Unmarshal(w.Body.Bytes(), &run); err != nil {
		t.Fatalf("decode apply-run begin: %v", err)
	}
	if strings.TrimSpace(run.ID) == "" {
		t.Fatal("apply-run id missing from begin response")
	}

	statusReq := httptest.NewRequest(http.MethodGet, "/v1/state-engine/apply-runs/"+run.ID+"/status", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, statusReq)
	if w.Code != http.StatusOK {
		t.Fatalf("apply-run status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	var statusResp struct {
		Status store.ApplyRunStatus `json:"status"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &statusResp); err != nil {
		t.Fatalf("decode apply-run status: %v", err)
	}
	if statusResp.Status != store.ApplyRunRunning {
		t.Fatalf("apply-run status = %q, want running", statusResp.Status)
	}

	finishReq := httptest.NewRequest(http.MethodPost, "/v1/state-engine/apply-runs/"+run.ID+"/finish", strings.NewReader(`{
	  "status":"committed",
	  "committed_serial":2,
	  "resources_planned":1,
	  "resources_applied":1
	}`))
	finishReq.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, finishReq)
	if w.Code != http.StatusOK {
		t.Fatalf("apply-run finish status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}

	statusReq = httptest.NewRequest(http.MethodGet, "/v1/state-engine/apply-runs/"+run.ID+"/status", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, statusReq)
	if w.Code != http.StatusOK {
		t.Fatalf("apply-run status after finish = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &statusResp); err != nil {
		t.Fatalf("decode finished apply-run status: %v", err)
	}
	if statusResp.Status != store.ApplyRunCommitted {
		t.Fatalf("apply-run status after finish = %q, want committed", statusResp.Status)
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
