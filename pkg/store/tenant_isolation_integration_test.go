//go:build integration

// Cross-tenant isolation tests. The single most important invariant
// of the multi-tenancy retrofit is "Tenant A cannot see, read, or
// modify Tenant B's data". This file proves that invariant against a
// live database — every other tenancy-related test happens inside a
// single tenant and would miss the failure mode where the
// `WHERE tenant_id = $N` filter on a store query gets dropped during
// a future refactor. These tests exist to break the day that
// regression lands, not the day it ships to a customer.
//
// The pattern in each test: create a second non-singleton tenant
// directly via SQL (the application has no API for tenant creation
// yet, because hosted-mode flows haven't landed), seed one piece of
// data per tenant under the same name, then prove that store calls
// under one tenant's ctx never see the other tenant's data.

package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kilolockio/kilolock/pkg/auth"
	"github.com/kilolockio/kilolock/pkg/testdb"
)

// otherTenantID is the second tenant used throughout this file. Hardcoded
// (rather than gen_random_uuid()) so a failing test's diagnostics name
// the tenant directly. Never overlaps with the singleton self-hosted id.
const otherTenantID = "11111111-1111-1111-1111-111111111111"

// seedOtherTenant inserts the second tenant if it doesn't already
// exist. Idempotent so repeated test runs are safe; the FK from
// states.tenant_id to tenants(id) won't accept a write under
// otherTenantID until this row exists.
func seedOtherTenant(t *testing.T, s *Store) {
	t.Helper()
	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Second)
	defer cancel()
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO tenants (id, slug, name)
		 VALUES ($1, 'other-tenant', 'Other Tenant (test fixture)')
		 ON CONFLICT (id) DO NOTHING`,
		otherTenantID,
	); err != nil {
		t.Fatalf("seed other tenant: %v", err)
	}
}

// ctxFor returns a context wrapped with a Principal for tenantID.
// Used by the per-tenant store-call sites in this file.
func ctxFor(t *testing.T, tenantID string) (context.Context, context.CancelFunc) {
	t.Helper()
	parent := auth.WithPrincipal(context.Background(), auth.Principal{
		TenantID: tenantID,
		Source:   "test",
	})
	return context.WithTimeout(parent, 10*time.Second)
}

// TestTenantIsolation_SameNameDifferentTenants pins the headline
// invariant: two tenants can each own a state called "prod", and
// neither sees the other. This is the multi-customer SaaS shape;
// "name uniqueness per tenant" was the entire reason migration 0009
// dropped the bare states_name_key constraint.
func TestTenantIsolation_SameNameDifferentTenants(t *testing.T) {
	s, pool := openTestStore(t)
	defer pool.Close()
	mustResetTables(t, pool)
	seedOtherTenant(t, s)

	const stateName = "prod"
	tfA := []byte(`{
		"version": 4, "terraform_version": "1.13.4",
		"serial": 1, "lineage": "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
		"outputs": {}, "resources": []
	}`)
	tfB := []byte(`{
		"version": 4, "terraform_version": "1.13.4",
		"serial": 1, "lineage": "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
		"outputs": {}, "resources": []
	}`)

	// Tenant A (the singleton): write a "prod" state with lineage A.
	ctxA, cancelA := ctxFor(t, auth.SelfHostedTenantID)
	defer cancelA()
	if err := s.WriteState(ctxA, stateName, "", tfA, "test", "tenant-a-actor"); err != nil {
		t.Fatalf("WriteState as tenant A: %v", err)
	}

	// Tenant B: write its OWN "prod" with a different lineage. The
	// old states_name_key constraint would have rejected this with
	// a unique violation; the (tenant_id, name) constraint accepts
	// it because the tuple is different.
	ctxB, cancelB := ctxFor(t, otherTenantID)
	defer cancelB()
	if err := s.WriteState(ctxB, stateName, "", tfB, "test", "tenant-b-actor"); err != nil {
		t.Fatalf("WriteState as tenant B (same name, different tenant should be allowed): %v", err)
	}

	// Tenant A reading "prod" must see lineage A, not B.
	rawA, err := s.GetCurrentState(ctxA, stateName)
	if err != nil {
		t.Fatalf("GetCurrentState as tenant A: %v", err)
	}
	if want := `"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"`; !contains(string(rawA), want) {
		t.Errorf("tenant A read leaked across tenants: state body did not contain lineage A (%s)", want)
	}
	if other := `"bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"`; contains(string(rawA), other) {
		t.Errorf("tenant A read leaked across tenants: state body contained tenant B's lineage (%s)", other)
	}

	// And the symmetric check: tenant B sees B's lineage, not A's.
	rawB, err := s.GetCurrentState(ctxB, stateName)
	if err != nil {
		t.Fatalf("GetCurrentState as tenant B: %v", err)
	}
	if want := `"bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"`; !contains(string(rawB), want) {
		t.Errorf("tenant B read leaked across tenants: state body did not contain lineage B (%s)", want)
	}
}

// TestTenantIsolation_ListStatesIsTenantScoped: ListStates is the
// most-likely user-facing surface for a cross-tenant leak — the
// CLI's `kl list` command — so the SQL has to filter by
// tenant_id and this test pins that contract.
func TestTenantIsolation_ListStatesIsTenantScoped(t *testing.T) {
	s, pool := openTestStore(t)
	defer pool.Close()
	mustResetTables(t, pool)
	seedOtherTenant(t, s)

	tfBody := []byte(`{
		"version": 4, "terraform_version": "1.13.4",
		"serial": 1, "lineage": "cccccccc-cccc-cccc-cccc-cccccccccccc",
		"outputs": {}, "resources": []
	}`)

	ctxA, cancelA := ctxFor(t, auth.SelfHostedTenantID)
	defer cancelA()
	if err := s.WriteState(ctxA, "a-state", "", tfBody, "test", "actor"); err != nil {
		t.Fatalf("write a-state as tenant A: %v", err)
	}

	ctxB, cancelB := ctxFor(t, otherTenantID)
	defer cancelB()
	if err := s.WriteState(ctxB, "b-state", "", tfBody, "test", "actor"); err != nil {
		t.Fatalf("write b-state as tenant B: %v", err)
	}

	got, err := s.ListStates(ctxA)
	if err != nil {
		t.Fatalf("ListStates: %v", err)
	}
	var names []string
	for _, st := range got {
		names = append(names, st.Name)
	}
	if !containsString(names, "a-state") {
		t.Errorf("tenant A's ListStates did not include its own state; got %v", names)
	}
	if containsString(names, "b-state") {
		t.Errorf("tenant A's ListStates leaked tenant B's state; got %v", names)
	}
}

// TestTenantIsolation_DeleteCannotReachOtherTenant: DELETE is the
// destructive surface, and a cross-tenant DELETE is the worst
// possible bug. This test proves a DeleteState call by tenant A
// against a name that exists in tenant B returns ErrStateNotFound
// rather than removing tenant B's row.
func TestTenantIsolation_DeleteCannotReachOtherTenant(t *testing.T) {
	s, pool := openTestStore(t)
	defer pool.Close()
	mustResetTables(t, pool)
	seedOtherTenant(t, s)

	tfBody := []byte(`{
		"version": 4, "terraform_version": "1.13.4",
		"serial": 1, "lineage": "dddddddd-dddd-dddd-dddd-dddddddddddd",
		"outputs": {}, "resources": []
	}`)

	ctxB, cancelB := ctxFor(t, otherTenantID)
	defer cancelB()
	const stateName = "tenant-b-secret"
	if err := s.WriteState(ctxB, stateName, "", tfBody, "test", "actor"); err != nil {
		t.Fatalf("write tenant B's state: %v", err)
	}

	// Tenant A tries to delete by the same name. Should return
	// ErrStateNotFound, not succeed silently.
	ctxA, cancelA := ctxFor(t, auth.SelfHostedTenantID)
	defer cancelA()
	err := s.DeleteState(ctxA, stateName, "", "tenant-a-actor")
	if !errors.Is(err, ErrStateNotFound) {
		t.Fatalf("DeleteState as wrong tenant: err = %v, want ErrStateNotFound", err)
	}

	// Tenant B's state must still exist.
	if _, err := s.GetCurrentState(ctxB, stateName); err != nil {
		t.Errorf("tenant B's state vanished after a cross-tenant DELETE attempt: %v", err)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
outer:
	for i := 0; i+len(needle) <= len(haystack); i++ {
		for j := 0; j < len(needle); j++ {
			if haystack[i+j] != needle[j] {
				continue outer
			}
		}
		return i
	}
	return -1
}

func containsString(slice []string, want string) bool {
	for _, s := range slice {
		if s == want {
			return true
		}
	}
	return false
}
