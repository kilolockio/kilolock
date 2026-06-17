//go:build integration

// Integration tests for current_resource_drift + Store.ListCurrentDrift
// (v1.7b). Run with:
//
//	KL_DATABASE_URL=postgres://kl:kl@localhost:5432/kl?sslmode=disable \
//	  go test -tags=integration ./pkg/store/...

package store

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/davesade/kilolock/internal/testdb"
	"strconv"
	"strings"
	"testing"
	"time"
)

// writeStateAt is a thin helper around WriteState that lets the
// test specify the source string explicitly (apply, refresh, ...).
// The drift view filters on state_versions.source = 'refresh', so
// the test needs to be able to mint both kinds of writes.
func writeStateAt(t *testing.T, s *Store, name, body, source string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 10*time.Second)
	defer cancel()
	if err := s.WriteState(ctx, name, "", []byte(body), source, "test"); err != nil {
		t.Fatalf("WriteState (%s): %v", source, err)
	}
}

// TestListCurrentDrift_NoState_ReturnsErr is the obvious gate: bogus
// state name surfaces ErrStateNotFound so demo/CLI code can give an
// honest "this state doesn't exist" message rather than an empty
// "no drift" success.
func TestListCurrentDrift_NoState_ReturnsErr(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	mustResetTables(t, pool)

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Second)
	defer cancel()

	_, err := s.ListCurrentDrift(ctx, "nope", 0)
	if !errors.Is(err, ErrStateNotFound) {
		t.Errorf("err: got %v, want ErrStateNotFound", err)
	}
}

// TestListCurrentDrift_FreshApply_NoDrift verifies that a state
// that has only ever seen `apply`-sourced writes returns zero
// drift rows. This is the baseline the demo opens with: applies
// don't produce drift, no matter how many of them there are.
func TestListCurrentDrift_FreshApply_NoDrift(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	mustResetTables(t, pool)

	const name = "drift-fresh"
	const v1 = `{
		"version": 4,
		"terraform_version": "1.13.4",
		"serial": 1,
		"lineage": "9b39e2c0-aaaa-bbbb-cccc-111122223333",
		"outputs": {},
		"resources": [
			{
				"mode": "managed",
				"type": "aws_vpc",
				"name": "main",
				"provider": "provider[\"registry.terraform.io/hashicorp/aws\"]",
				"instances": [{"schema_version": 0, "attributes": {"id": "vpc-1"}, "sensitive_attributes": []}]
			}
		]
	}`
	writeStateAt(t, s, name, v1, "apply")

	// A second apply-sourced write that changes attributes: still
	// not drift (drift, by definition, comes from refresh).
	const v2 = `{
		"version": 4,
		"terraform_version": "1.13.4",
		"serial": 2,
		"lineage": "9b39e2c0-aaaa-bbbb-cccc-111122223333",
		"outputs": {},
		"resources": [
			{
				"mode": "managed",
				"type": "aws_vpc",
				"name": "main",
				"provider": "provider[\"registry.terraform.io/hashicorp/aws\"]",
				"instances": [{"schema_version": 0, "attributes": {"id": "vpc-2"}, "sensitive_attributes": []}]
			}
		]
	}`
	writeStateAt(t, s, name, v2, "apply")

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Second)
	defer cancel()

	got, err := s.ListCurrentDrift(ctx, name, 0)
	if err != nil {
		t.Fatalf("ListCurrentDrift: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("drift rows on apply-only state: got %d, want 0", len(got))
	}
}

// TestListCurrentDrift_RefreshIntroducesDrift is the central
// behavior test: an apply writes attribute X="before", a refresh
// writes attribute X="after", current_resource_drift should expose
// exactly one row with current_attributes and previous_attributes
// reflecting the change.
func TestListCurrentDrift_RefreshIntroducesDrift(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	mustResetTables(t, pool)

	const name = "drift-detected"
	// v1: applied truth.
	const v1 = `{
		"version": 4,
		"terraform_version": "1.13.4",
		"serial": 1,
		"lineage": "9b39e2c0-aaaa-bbbb-cccc-222233334444",
		"outputs": {},
		"resources": [
			{
				"mode": "managed",
				"type": "aws_vpc",
				"name": "main",
				"provider": "provider[\"registry.terraform.io/hashicorp/aws\"]",
				"instances": [{"schema_version": 0, "attributes": {"id": "vpc-1", "tag": "before"}, "sensitive_attributes": []}]
			}
		]
	}`
	writeStateAt(t, s, name, v1, "apply")

	// v2: refresh sees a different tag (out-of-band change). This
	// is the canonical drift scenario.
	const v2 = `{
		"version": 4,
		"terraform_version": "1.13.4",
		"serial": 2,
		"lineage": "9b39e2c0-aaaa-bbbb-cccc-222233334444",
		"outputs": {},
		"resources": [
			{
				"mode": "managed",
				"type": "aws_vpc",
				"name": "main",
				"provider": "provider[\"registry.terraform.io/hashicorp/aws\"]",
				"instances": [{"schema_version": 0, "attributes": {"id": "vpc-1", "tag": "after"}, "sensitive_attributes": []}]
			}
		]
	}`
	writeStateAt(t, s, name, v2, "refresh")

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Second)
	defer cancel()

	got, err := s.ListCurrentDrift(ctx, name, 0)
	if err != nil {
		t.Fatalf("ListCurrentDrift: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("drift rows: got %d, want 1; got=%+v", len(got), got)
	}

	row := got[0]
	if row.Address != "aws_vpc.main" {
		t.Errorf("address: got %q", row.Address)
	}
	if row.StateName != name {
		t.Errorf("state_name: got %q", row.StateName)
	}
	if row.DetectedAtSerial != 2 {
		t.Errorf("detected_at_serial: got %d, want 2", row.DetectedAtSerial)
	}
	// current_attributes carries the post-refresh tag; previous
	// carries the pre-refresh tag. Use a structural comparison so
	// JSONB whitespace normalization isn't a flake source.
	if !jsonHasField(t, row.CurrentAttributes, "tag", "after") {
		t.Errorf("current_attributes missing tag=after: %s", string(row.CurrentAttributes))
	}
	if !jsonHasField(t, row.PreviousAttributes, "tag", "before") {
		t.Errorf("previous_attributes missing tag=before: %s", string(row.PreviousAttributes))
	}
}

// TestListCurrentDrift_ApplyClearsDrift exercises the lifecycle's
// "drift is resolved when the next apply lands" property: after a
// refresh creates a drift row, a subsequent apply that brings the
// state in line removes that row from the view (a new lifecycle
// opens with create_serial = apply_serial, and that source isn't
// 'refresh').
func TestListCurrentDrift_ApplyClearsDrift(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	mustResetTables(t, pool)

	const name = "drift-resolved"
	writeStateAt(t, s, name, mkState(1, "vpc-1", "before"), "apply")
	writeStateAt(t, s, name, mkState(2, "vpc-1", "after"), "refresh")

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Second)
	defer cancel()

	got, err := s.ListCurrentDrift(ctx, name, 0)
	if err != nil {
		t.Fatalf("ListCurrentDrift after refresh: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 drift row after refresh; got %d", len(got))
	}

	// An apply that re-asserts the new value closes the refresh-
	// lifecycle and opens a new one with source='apply'. The new
	// lifecycle isn't drift; the old one is closed; the view goes
	// empty.
	writeStateAt(t, s, name, mkState(3, "vpc-1", "after"), "apply")

	got, err = s.ListCurrentDrift(ctx, name, 0)
	if err != nil {
		t.Fatalf("ListCurrentDrift after apply: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("drift not cleared after reconciling apply: got %d rows", len(got))
	}
}

// TestListCurrentDrift_HonorsLimit verifies pagination: when many
// resources drift at once, ListCurrentDrift respects the limit so
// callers can stream a long drift report without pulling every
// row into memory.
func TestListCurrentDrift_HonorsLimit(t *testing.T) {
	s, pool := openTestStore(t)
	t.Cleanup(pool.Close)
	mustResetTables(t, pool)

	const name = "drift-many"
	writeStateAt(t, s, name, mkStateN(1, "before", 5), "apply")
	writeStateAt(t, s, name, mkStateN(2, "after", 5), "refresh")

	ctx, cancel := context.WithTimeout(testdb.BackgroundTenantCtx(), 5*time.Second)
	defer cancel()

	full, err := s.ListCurrentDrift(ctx, name, 0)
	if err != nil {
		t.Fatalf("ListCurrentDrift full: %v", err)
	}
	if len(full) != 5 {
		t.Fatalf("expected 5 drift rows; got %d", len(full))
	}

	limited, err := s.ListCurrentDrift(ctx, name, 2)
	if err != nil {
		t.Fatalf("ListCurrentDrift limited: %v", err)
	}
	if len(limited) != 2 {
		t.Errorf("expected 2 rows, got %d", len(limited))
	}
}

// mkState renders a one-resource Terraform v4 state body with the
// given serial / id / tag. Keeps the SQL fixtures readable.
func mkState(serial int, id, tag string) string {
	return strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(`{
		"version": 4,
		"terraform_version": "1.13.4",
		"serial": __SERIAL__,
		"lineage": "9b39e2c0-aaaa-bbbb-cccc-555566667777",
		"outputs": {},
		"resources": [
			{
				"mode": "managed",
				"type": "aws_vpc",
				"name": "main",
				"provider": "provider[\"registry.terraform.io/hashicorp/aws\"]",
				"instances": [{"schema_version": 0, "attributes": {"id": "__ID__", "tag": "__TAG__"}, "sensitive_attributes": []}]
			}
		]
	}`, "__SERIAL__", iToA(serial)), "__ID__", id), "__TAG__", tag)
}

// mkStateN renders a state with N null_resource_<i> resources, each
// carrying the same tag value. Used by the limit / multi-drift test.
func mkStateN(serial int, tag string, n int) string {
	var b strings.Builder
	b.WriteString(`{
		"version": 4,
		"terraform_version": "1.13.4",
		"serial": ` + iToA(serial) + `,
		"lineage": "9b39e2c0-aaaa-bbbb-cccc-888899990000",
		"outputs": {},
		"resources": [`)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`
			{
				"mode": "managed",
				"type": "null_resource",
				"name": "n` + iToA(i) + `",
				"provider": "provider[\"registry.terraform.io/hashicorp/null\"]",
				"instances": [{"schema_version": 0, "attributes": {"id": "n` + iToA(i) + `", "tag": "` + tag + `"}, "sensitive_attributes": []}]
			}`)
	}
	b.WriteString("]}")
	return b.String()
}

func iToA(n int) string { return strconv.Itoa(n) }

func jsonHasField(t *testing.T, raw json.RawMessage, key, want string) bool {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Errorf("unmarshal %s: %v", string(raw), err)
		return false
	}
	got, ok := m[key].(string)
	return ok && got == want
}
