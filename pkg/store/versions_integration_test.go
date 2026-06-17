//go:build integration

// Run with:
//
//	KL_DATABASE_URL=postgres://kl:kl@localhost:5432/kl?sslmode=disable \
//	  go test -tags=integration -run 'TestVersions|TestReplayVersion|TestDiffVersionAddresses' ./pkg/store/...

package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kilolockio/kilolock/internal/tfstate"
	"github.com/kilolockio/kilolock/pkg/testdb"
)

// seedStateWithResources writes one state_version for "qtest" carrying
// the supplied set of resource addresses (each modelled as a minimal
// aws_instance with a single attribute we can mutate per version to
// exercise the diff classifier). Used to build a multi-version
// fixture for the diff + rollback tests below.
func seedStateWithResources(t *testing.T, s *Store, name string, serial int, addresses map[string]string) {
	t.Helper()
	resources := make([]any, 0, len(addresses))
	for addr, attrID := range addresses {
		// Each address is type.name; split on '.' once so we
		// model it correctly in the v4 statefile schema.
		typ, n, ok := splitTypeName(addr)
		if !ok {
			t.Fatalf("invalid address %q (want type.name)", addr)
		}
		resources = append(resources, map[string]any{
			"mode":     "managed",
			"type":     typ,
			"name":     n,
			"provider": `provider["registry.terraform.io/hashicorp/aws"]`,
			"instances": []any{
				map[string]any{
					"schema_version": 0,
					// The attr id varies per version so the diff
					// classifier has something to compare on the
					// "changed" branch.
					"attributes":           map[string]any{"id": attrID},
					"sensitive_attributes": []any{},
				},
			},
		})
	}
	body := map[string]any{
		"version":           4,
		"terraform_version": "1.13.4",
		"serial":            serial,
		"lineage":           "9b39e2c0-aaaa-bbbb-cccc-444455556666",
		"outputs":           map[string]any{},
		"resources":         resources,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal seed state: %v", err)
	}
	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 10*time.Second)
	defer cancel()
	if err := s.WriteState(ctx, name, "", raw, "test", "tester"); err != nil {
		t.Fatalf("seed write serial=%d: %v", serial, err)
	}
}

func splitTypeName(addr string) (typ, name string, ok bool) {
	for i := 0; i < len(addr); i++ {
		if addr[i] == '.' {
			return addr[:i], addr[i+1:], true
		}
	}
	return "", "", false
}

// ---------------------------------------------------------------------------
// ListVersions + GetVersionRaw
// ---------------------------------------------------------------------------

func TestVersions_ListAndGet(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	mustResetTables(t, pool)

	seedStateWithResources(t, s, "qtest", 1, map[string]string{"aws_vpc.main": "vpc-1"})
	seedStateWithResources(t, s, "qtest", 2, map[string]string{
		"aws_vpc.main":     "vpc-1",
		"aws_instance.web": "i-100",
	})
	seedStateWithResources(t, s, "qtest", 3, map[string]string{
		"aws_vpc.main":     "vpc-1",
		"aws_instance.web": "i-100",
		"aws_instance.db":  "i-200",
	})

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 10*time.Second)
	defer cancel()

	versions, err := s.ListVersions(ctx, "qtest", 0, 0)
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(versions) != 3 {
		t.Fatalf("got %d versions, want 3", len(versions))
	}
	// Newest first.
	if versions[0].Serial != 3 || versions[2].Serial != 1 {
		t.Errorf("ordering: got serials %d,%d,%d; want 3,2,1",
			versions[0].Serial, versions[1].Serial, versions[2].Serial)
	}
	// IsCurrent must mark exactly one row.
	currentCount := 0
	for _, v := range versions {
		if v.IsCurrent {
			currentCount++
			if v.Serial != 3 {
				t.Errorf("current marked on serial %d; want 3", v.Serial)
			}
		}
	}
	if currentCount != 1 {
		t.Errorf("IsCurrent marked on %d rows; want 1", currentCount)
	}

	// limit + offset round-trip.
	page, err := s.ListVersions(ctx, "qtest", 2, 1)
	if err != nil {
		t.Fatalf("ListVersions(limit=2,offset=1): %v", err)
	}
	if len(page) != 2 || page[0].Serial != 2 || page[1].Serial != 1 {
		t.Errorf("pagination: got serials %v; want [2,1]", serialsOf(page))
	}

	// Reference resolution: every shape resolves to the same row
	// when given the same logical version.
	v2byCurrentMinus1 := mustResolve(t, s, "qtest", "@1")
	v2bySerial := mustResolve(t, s, "qtest", "2")
	v2byUUID := mustResolve(t, s, "qtest", v2byCurrentMinus1.ID)
	if v2byCurrentMinus1.ID != v2bySerial.ID || v2byCurrentMinus1.ID != v2byUUID.ID {
		t.Errorf("ref shapes diverged: @1=%s serial=%s uuid=%s",
			v2byCurrentMinus1.ID, v2bySerial.ID, v2byUUID.ID)
	}

	// "current" alias.
	cur := mustResolve(t, s, "qtest", "current")
	if cur.Serial != 3 || !cur.IsCurrent {
		t.Errorf("current ref returned serial=%d isCurrent=%v; want 3,true", cur.Serial, cur.IsCurrent)
	}

	// Invalid refs are caller errors.
	_, _, err = s.GetVersionRaw(ctx, "qtest", "@-1")
	if err == nil {
		t.Errorf("expected error on @-1")
	}
	_, _, err = s.GetVersionRaw(ctx, "qtest", "not-a-ref")
	if err == nil {
		t.Errorf("expected error on garbage ref")
	}
}

// ---------------------------------------------------------------------------
// DiffVersionAddresses
// ---------------------------------------------------------------------------

func TestDiffVersionAddresses_AddedRemovedChanged(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	mustResetTables(t, pool)

	// v1: {vpc.main(id=vpc-1), instance.web(id=i-100)}
	seedStateWithResources(t, s, "qtest", 1, map[string]string{
		"aws_vpc.main":     "vpc-1",
		"aws_instance.web": "i-100",
	})
	// v2: {vpc.main(id=vpc-1)  unchanged,
	//      instance.web(id=i-200)  attrs changed,
	//      instance.db(id=i-300)   added,
	//      and instance.web's attrs change}
	// (instance.cache went away — wait, we didn't add it in v1; let's
	// keep a "removed" case by dropping instance.web in v3 instead.)
	seedStateWithResources(t, s, "qtest", 2, map[string]string{
		"aws_vpc.main":     "vpc-1",
		"aws_instance.web": "i-200",
		"aws_instance.db":  "i-300",
	})
	// v3: instance.web is gone; that's our removed.
	seedStateWithResources(t, s, "qtest", 3, map[string]string{
		"aws_vpc.main":    "vpc-1",
		"aws_instance.db": "i-300",
	})

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 10*time.Second)
	defer cancel()

	v1 := mustResolve(t, s, "qtest", "1")
	v2 := mustResolve(t, s, "qtest", "2")
	v3 := mustResolve(t, s, "qtest", "3")

	// v1 → v2: web's attrs changed; db is added; vpc unchanged.
	d12, err := s.DiffVersionAddresses(ctx, v1.ID, v2.ID)
	if err != nil {
		t.Fatalf("diff v1→v2: %v", err)
	}
	if !equalSorted(d12.Added, []string{"aws_instance.db"}) {
		t.Errorf("v1→v2 Added = %v; want [aws_instance.db]", d12.Added)
	}
	if len(d12.Removed) != 0 {
		t.Errorf("v1→v2 Removed = %v; want []", d12.Removed)
	}
	if !equalSorted(d12.Changed, []string{"aws_instance.web"}) {
		t.Errorf("v1→v2 Changed = %v; want [aws_instance.web]", d12.Changed)
	}

	// v2 → v3: web removed; db unchanged; vpc unchanged.
	d23, err := s.DiffVersionAddresses(ctx, v2.ID, v3.ID)
	if err != nil {
		t.Fatalf("diff v2→v3: %v", err)
	}
	if !equalSorted(d23.Removed, []string{"aws_instance.web"}) {
		t.Errorf("v2→v3 Removed = %v; want [aws_instance.web]", d23.Removed)
	}
	if len(d23.Added) != 0 || len(d23.Changed) != 0 {
		t.Errorf("v2→v3 expected only Removed; got %+v", d23)
	}

	// Self-diff is empty.
	dself, err := s.DiffVersionAddresses(ctx, v3.ID, v3.ID)
	if err != nil {
		t.Fatalf("diff v3→v3: %v", err)
	}
	if len(dself.Added)+len(dself.Removed)+len(dself.Changed) != 0 {
		t.Errorf("self-diff non-empty: %+v", dself)
	}
}

// ---------------------------------------------------------------------------
// DiffVersionResources (attribute-level)
// ---------------------------------------------------------------------------

// TestDiffVersionResources_ProjectsAttributesAndStatuses pins the
// per-row contract of the attribute-level diff: each changed/added/
// removed address appears once, with FromAttrs and ToAttrs populated
// appropriately for its status. The address-level companion test
// already proves which addresses end up in which bucket; this test
// proves the BYTES land in the right fields.
func TestDiffVersionResources_ProjectsAttributesAndStatuses(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	mustResetTables(t, pool)

	seedStateWithResources(t, s, "qtest", 1, map[string]string{
		"aws_vpc.main":     "vpc-1",
		"aws_instance.web": "i-100",
	})
	seedStateWithResources(t, s, "qtest", 2, map[string]string{
		"aws_vpc.main":     "vpc-1", // unchanged
		"aws_instance.web": "i-200", // changed
		"aws_instance.db":  "i-300", // added
	})

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 10*time.Second)
	defer cancel()

	v1 := mustResolve(t, s, "qtest", "1")
	v2 := mustResolve(t, s, "qtest", "2")

	rows, err := s.DiffVersionResources(ctx, v1.ID, v2.ID)
	if err != nil {
		t.Fatalf("DiffVersionResources: %v", err)
	}

	byAddr := map[string]ResourceAttrDelta{}
	for _, r := range rows {
		byAddr[r.Address] = r
	}

	// The unchanged vpc must NOT appear.
	if _, found := byAddr["aws_vpc.main"]; found {
		t.Errorf("unchanged aws_vpc.main appeared in diff (should be filtered)")
	}

	// aws_instance.db is added: FromAttrs nil, ToAttrs populated.
	if r, ok := byAddr["aws_instance.db"]; !ok {
		t.Errorf("aws_instance.db missing from diff")
	} else {
		if r.Status != "added" {
			t.Errorf("aws_instance.db status = %q, want added", r.Status)
		}
		if len(r.FromAttrs) > 0 && string(r.FromAttrs) != "null" {
			t.Errorf("aws_instance.db FromAttrs should be nil/null, got %s", r.FromAttrs)
		}
		if len(r.ToAttrs) == 0 {
			t.Errorf("aws_instance.db ToAttrs should be populated")
		}
	}

	// aws_instance.web is changed: both sides populated, attrs distinct.
	if r, ok := byAddr["aws_instance.web"]; !ok {
		t.Errorf("aws_instance.web missing from diff")
	} else {
		if r.Status != "changed" {
			t.Errorf("aws_instance.web status = %q, want changed", r.Status)
		}
		if len(r.FromAttrs) == 0 || len(r.ToAttrs) == 0 {
			t.Errorf("aws_instance.web both sides should be populated, got from=%s to=%s", r.FromAttrs, r.ToAttrs)
		}
		if string(r.FromAttrs) == string(r.ToAttrs) {
			t.Errorf("aws_instance.web from/to attrs identical, want distinct")
		}
	}
}

// ---------------------------------------------------------------------------
// ReplayVersion (rollback)
// ---------------------------------------------------------------------------

func TestReplayVersion_RollbackIsAppendOnlyAndProjectsCorrectly(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	mustResetTables(t, pool)

	// History:
	//   serial 1:  vpc.main(vpc-1)                                  ← target
	//   serial 2:  vpc.main(vpc-1), instance.web(i-100)
	//   serial 3:  vpc.main(vpc-1), instance.web(i-100), instance.db(i-300)
	seedStateWithResources(t, s, "qtest", 1, map[string]string{
		"aws_vpc.main": "vpc-1",
	})
	seedStateWithResources(t, s, "qtest", 2, map[string]string{
		"aws_vpc.main":     "vpc-1",
		"aws_instance.web": "i-100",
	})
	seedStateWithResources(t, s, "qtest", 3, map[string]string{
		"aws_vpc.main":     "vpc-1",
		"aws_instance.web": "i-100",
		"aws_instance.db":  "i-300",
	})

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 10*time.Second)
	defer cancel()

	target, err := s.ReplayVersion(ctx, "qtest", "1", "alice@host")
	if err != nil {
		t.Fatalf("ReplayVersion: %v", err)
	}

	// New version must sit at MAX(serial)+1.
	if target.Serial != 4 {
		t.Errorf("new serial = %d; want 4", target.Serial)
	}
	if target.Source != "rollback" {
		t.Errorf("source = %q; want rollback", target.Source)
	}
	if target.CreatedBy != "alice@host" {
		t.Errorf("created_by = %q; want alice@host", target.CreatedBy)
	}
	if !target.IsCurrent {
		t.Errorf("IsCurrent = false on the new rollback version")
	}

	// History is now 4 rows; none of the original 3 was removed.
	versions, err := s.ListVersions(ctx, "qtest", 0, 0)
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(versions) != 4 {
		t.Fatalf("len(versions) = %d; want 4 (rollback must NOT delete history)", len(versions))
	}

	// Resource projection now reflects the v1 snapshot — instance.web
	// and instance.db must have been "rolled away" from the current
	// view but their previous-version rows must still exist.
	rolled, err := s.ListVersions(ctx, "qtest", 1, 0)
	if err != nil {
		t.Fatalf("ListVersions(latest): %v", err)
	}
	if !rolled[0].IsCurrent || rolled[0].Serial != 4 {
		t.Errorf("latest after rollback: serial=%d current=%v; want 4,true",
			rolled[0].Serial, rolled[0].IsCurrent)
	}

	// Diff the rollback against the pre-rollback head: instance.web
	// and instance.db should appear as Removed.
	preRollback := mustResolve(t, s, "qtest", "3")
	d, err := s.DiffVersionAddresses(ctx, preRollback.ID, target.ID)
	if err != nil {
		t.Fatalf("diff preRollback→rollback: %v", err)
	}
	if !equalSorted(d.Removed, []string{"aws_instance.db", "aws_instance.web"}) {
		t.Errorf("rollback diff Removed = %v; want [aws_instance.db, aws_instance.web]", d.Removed)
	}
	if len(d.Added) != 0 || len(d.Changed) != 0 {
		t.Errorf("rollback diff Added/Changed non-empty: %+v", d)
	}

	// The new state_version's raw_state must carry the rewritten
	// serial. Without that rewrite, Terraform on next read would
	// see an internal "serial: 1" payload and panic on serial
	// mismatch.
	_, raw, err := s.GetVersionRaw(ctx, "qtest", target.ID)
	if err != nil {
		t.Fatalf("GetVersionRaw(target): %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	// json decoded serial comes back as float64 by default; compare
	// via float, not int.
	if got, _ := doc["serial"].(float64); int64(got) != target.Serial {
		t.Errorf("raw_state.serial = %v; want %d", doc["serial"], target.Serial)
	}

	// Lineage must be preserved (rollback is the SAME state, not a
	// new one). Without lineage preservation, Terraform refuses the
	// next write with a "lineage changed" error.
	if got, _ := doc["lineage"].(string); got != "9b39e2c0-aaaa-bbbb-cccc-444455556666" {
		t.Errorf("lineage mutated: %v", got)
	}

	// Audit: an events row must have been written for this rollback.
	var eventCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM events
		 WHERE kind = 'state_rollback'
		   AND state_version_id = $1`,
		target.ID,
	).Scan(&eventCount); err != nil {
		t.Fatalf("events count: %v", err)
	}
	if eventCount != 1 {
		t.Errorf("state_rollback event count = %d; want 1", eventCount)
	}
}

func TestReplayVersion_RejectsUnknownState(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	mustResetTables(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Second)
	defer cancel()

	_, err := s.ReplayVersion(ctx, "does-not-exist", "1", "test")
	if !errors.Is(err, ErrStateNotFound) {
		t.Errorf("got %v; want ErrStateNotFound", err)
	}
}

func TestReplayVersion_RejectsUnknownVersion(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	mustResetTables(t, pool)
	seedStateWithResources(t, s, "qtest", 1, map[string]string{"aws_vpc.main": "vpc-1"})

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Second)
	defer cancel()

	_, err := s.ReplayVersion(ctx, "qtest", "999", "test")
	if !errors.Is(err, ErrStateNotFound) {
		t.Errorf("got %v; want ErrStateNotFound for missing serial", err)
	}
}

func TestReplayResourceVersion_ReplaysSingleAddressOnly(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	mustResetTables(t, pool)

	seedStateWithResources(t, s, "qtest", 1, map[string]string{
		"aws_vpc.main":     "vpc-1",
		"aws_instance.web": "i-100",
	})
	seedStateWithResources(t, s, "qtest", 2, map[string]string{
		"aws_vpc.main":     "vpc-1",
		"aws_instance.web": "i-200",
		"aws_instance.db":  "i-300",
	})

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 10*time.Second)
	defer cancel()

	newVersion, preview, err := s.ReplayResourceVersion(ctx, "qtest", "aws_instance.web", "1", "alice@host")
	if err != nil {
		t.Fatalf("ReplayResourceVersion: %v", err)
	}
	if preview == nil {
		t.Fatalf("preview = nil")
	}
	if preview.Action != "replace" {
		t.Fatalf("preview.Action = %q, want replace", preview.Action)
	}
	if newVersion == nil {
		t.Fatalf("newVersion = nil")
	}
	if newVersion.Serial != 3 {
		t.Fatalf("new serial = %d, want 3", newVersion.Serial)
	}

	current, raw, err := s.GetVersionRaw(ctx, "qtest", "current")
	if err != nil {
		t.Fatalf("GetVersionRaw(current): %v", err)
	}
	if current.Serial != 3 {
		t.Fatalf("current serial = %d, want 3", current.Serial)
	}

	state, err := tfstate.Parse(raw)
	if err != nil {
		t.Fatalf("parse current state: %v", err)
	}
	assertInstanceIDInState(t, state, "aws_instance.web", "i-100")
	assertInstanceIDInState(t, state, "aws_instance.db", "i-300")
	assertInstanceIDInState(t, state, "aws_vpc.main", "vpc-1")

	history, err := s.ListResourceHistory(ctx, "qtest", "aws_instance.web", 10)
	if err != nil {
		t.Fatalf("ListResourceHistory: %v", err)
	}
	if len(history) == 0 {
		t.Fatalf("resource history unexpectedly empty")
	}
	if history[0].CreateSerial != 3 {
		t.Fatalf("latest history create_serial = %d, want 3", history[0].CreateSerial)
	}
}

func TestPreviewReplayResourceVersion_UsesIndexedResourceSnapshots(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	mustResetTables(t, pool)

	seedStateWithResources(t, s, "qtest", 1, map[string]string{
		"aws_instance.web": "i-100",
	})
	seedStateWithResources(t, s, "qtest", 2, map[string]string{
		"aws_instance.web": "i-200",
		"aws_instance.db":  "i-300",
	})

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 10*time.Second)
	defer cancel()

	preview, err := s.PreviewReplayResourceVersion(ctx, "qtest", "aws_instance.web", "1")
	if err != nil {
		t.Fatalf("PreviewReplayResourceVersion: %v", err)
	}
	if preview.Action != "replace" {
		t.Fatalf("preview.Action = %q, want replace", preview.Action)
	}
	if !preview.CurrentExists || !preview.TargetExists {
		t.Fatalf("preview existence = current:%v target:%v, want both true", preview.CurrentExists, preview.TargetExists)
	}
	if !strings.Contains(string(preview.CurrentAttrs), `"i-200"`) {
		t.Fatalf("preview.CurrentAttrs = %s, want current id", preview.CurrentAttrs)
	}
	if !strings.Contains(string(preview.TargetAttrs), `"i-100"`) {
		t.Fatalf("preview.TargetAttrs = %s, want target id", preview.TargetAttrs)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func mustResolve(t *testing.T, s *Store, name, ref string) *StateVersionInfo {
	t.Helper()
	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Second)
	defer cancel()
	info, _, err := s.GetVersionRaw(ctx, name, ref)
	if err != nil {
		t.Fatalf("GetVersionRaw(%q, %q): %v", name, ref, err)
	}
	return info
}

func serialsOf(vs []StateVersionInfo) []int64 {
	out := make([]int64, len(vs))
	for i, v := range vs {
		out[i] = v.Serial
	}
	return out
}

func equalSorted(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func assertInstanceIDInState(t *testing.T, st *tfstate.State, address, want string) {
	t.Helper()
	loc, err := findResourceInstance(st, address)
	if err != nil {
		t.Fatalf("findResourceInstance(%q): %v", address, err)
	}
	if loc == nil {
		t.Fatalf("address %q missing from state", address)
	}
	var attrs map[string]any
	if err := json.Unmarshal(loc.Instance.Attributes, &attrs); err != nil {
		t.Fatalf("decode attrs for %q: %v", address, err)
	}
	if got, _ := attrs["id"].(string); got != want {
		t.Fatalf("%s id = %q, want %q", address, got, want)
	}
}
