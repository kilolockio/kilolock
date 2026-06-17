package store

import (
	"encoding/json"
	"errors"
	"sort"
	"testing"

	"github.com/kilolockio/kilolock/internal/tfstate"
)

// helpers --------------------------------------------------------------

// mergeInstance is a one-liner for building a single resource
// instance with a JSON-encoded attribute blob. The instance has no
// index_key, no dependencies, no sensitive paths — just enough to
// make the merge logic compare attribute payloads.
func mergeInstance(attr string) tfstate.ResourceInstance {
	return tfstate.ResourceInstance{
		Attributes: json.RawMessage(attr),
	}
}

// mergeResource builds a single-instance Resource group.
func mergeResource(typ, name, attr string) tfstate.Resource {
	return tfstate.Resource{
		Mode:      "managed",
		Type:      typ,
		Name:      name,
		Provider:  `provider["registry.terraform.io/hashicorp/test"]`,
		Instances: []tfstate.ResourceInstance{mergeInstance(attr)},
	}
}

// mergeStateOf assembles a minimal v4 state. Serial and Lineage are
// passed in so each test can declare its own base/trunk/proposed
// triple.
func mergeStateOf(lineage string, serial int64, resources ...tfstate.Resource) *tfstate.State {
	return &tfstate.State{
		Version:          4,
		TerraformVersion: "1.6.0",
		Serial:           serial,
		Lineage:          lineage,
		Outputs:          map[string]tfstate.Output{},
		Resources:        resources,
	}
}

// addressesOf returns the sorted set of "type.name" addresses
// present in a state. Used to assert merge results.
func addressesOf(s *tfstate.State) []string {
	out := []string{}
	for _, r := range s.Resources {
		out = append(out, r.Type+"."+r.Name)
	}
	sort.Strings(out)
	return out
}

// instanceAttrs returns the attribute JSON for "type.name".
// Lookup-by-address scoped to tests (the merge map already keys
// by full address; here we just need the round-tripped attrs).
func instanceAttrs(t *testing.T, s *tfstate.State, addr string) string {
	t.Helper()
	for _, r := range s.Resources {
		if r.Type+"."+r.Name == addr {
			if len(r.Instances) != 1 {
				t.Fatalf("instanceAttrs(%q): expected 1 instance, got %d", addr, len(r.Instances))
			}
			return string(r.Instances[0].Attributes)
		}
	}
	t.Fatalf("instanceAttrs(%q): not found", addr)
	return ""
}

// merge tests ----------------------------------------------------------

// TestMerge_DisjointWrites_Succeed exercises the happy path Option A
// is built for: two operators touch disjoint addresses, both should
// be visible in the merged result.
func TestMerge_DisjointWrites_Succeed(t *testing.T) {
	base := mergeStateOf("L1", 10,
		mergeResource("aws_instance", "a", `{"v":1}`),
		mergeResource("aws_instance", "b", `{"v":1}`),
		mergeResource("aws_instance", "c", `{"v":1}`),
	)
	// Operator T (trunk) bumped only "a".
	trunk := mergeStateOf("L1", 11,
		mergeResource("aws_instance", "a", `{"v":2}`),
		mergeResource("aws_instance", "b", `{"v":1}`),
		mergeResource("aws_instance", "c", `{"v":1}`),
	)
	// Operator P (proposed) bumped only "b".
	proposed := mergeStateOf("L1", 11,
		mergeResource("aws_instance", "a", `{"v":1}`),
		mergeResource("aws_instance", "b", `{"v":99}`),
		mergeResource("aws_instance", "c", `{"v":1}`),
	)

	merged, err := MergeStates(base, trunk, proposed)
	if err != nil {
		t.Fatalf("MergeStates: unexpected error %v", err)
	}
	if got, want := merged.Serial, int64(12); got != want {
		t.Errorf("merged.Serial = %d, want %d", got, want)
	}
	if got, want := merged.Lineage, "L1"; got != want {
		t.Errorf("merged.Lineage = %q, want %q", got, want)
	}
	if got, want := addressesOf(merged), []string{"aws_instance.a", "aws_instance.b", "aws_instance.c"}; !sliceEqual(got, want) {
		t.Errorf("merged addresses = %v, want %v", got, want)
	}
	// a should come from trunk (v=2), b from proposed (v=99), c
	// from either (both same).
	if got, want := instanceAttrs(t, merged, "aws_instance.a"), `{"v":2}`; got != want {
		t.Errorf("merged a attrs = %s, want %s", got, want)
	}
	if got, want := instanceAttrs(t, merged, "aws_instance.b"), `{"v":99}`; got != want {
		t.Errorf("merged b attrs = %s, want %s", got, want)
	}
}

// TestMerge_OverlappingWrites_Conflict catches the case Option A
// is supposed to reject: both operators wrote the same address.
func TestMerge_OverlappingWrites_Conflict(t *testing.T) {
	base := mergeStateOf("L1", 10,
		mergeResource("aws_instance", "a", `{"v":1}`),
	)
	trunk := mergeStateOf("L1", 11,
		mergeResource("aws_instance", "a", `{"v":2}`),
	)
	proposed := mergeStateOf("L1", 11,
		mergeResource("aws_instance", "a", `{"v":99}`),
	)

	_, err := MergeStates(base, trunk, proposed)
	var conflict *WriteSetConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("MergeStates: want WriteSetConflictError, got %v", err)
	}
	if got, want := conflict.Addresses, []string{"aws_instance.a"}; !sliceEqual(got, want) {
		t.Errorf("conflict.Addresses = %v, want %v", got, want)
	}
	if got, want := conflict.LatestSerial, int64(11); got != want {
		t.Errorf("conflict.LatestSerial = %d, want %d", got, want)
	}
}

// TestMerge_AddedOnEachSide_Disjoint covers the "two new resources
// on disjoint addresses" pattern — common when two engineers add
// new infra concurrently.
func TestMerge_AddedOnEachSide_Disjoint(t *testing.T) {
	base := mergeStateOf("L1", 10)
	trunk := mergeStateOf("L1", 11,
		mergeResource("aws_instance", "a", `{"v":1}`),
	)
	proposed := mergeStateOf("L1", 11,
		mergeResource("aws_instance", "b", `{"v":1}`),
	)

	merged, err := MergeStates(base, trunk, proposed)
	if err != nil {
		t.Fatalf("MergeStates: unexpected error %v", err)
	}
	if got, want := addressesOf(merged), []string{"aws_instance.a", "aws_instance.b"}; !sliceEqual(got, want) {
		t.Errorf("merged addresses = %v, want %v", got, want)
	}
}

// TestMerge_RemovedByMe_KeptByOther: operator P deleted address X;
// operator T did NOT touch it. Result: X is removed.
//
// This is the canonical "destroy doesn't conflict if nobody else
// touched the resource" case.
func TestMerge_RemovedByMe_KeptByOther(t *testing.T) {
	base := mergeStateOf("L1", 10,
		mergeResource("aws_instance", "a", `{"v":1}`),
		mergeResource("aws_instance", "b", `{"v":1}`),
	)
	trunk := mergeStateOf("L1", 10, // trunk unchanged
		mergeResource("aws_instance", "a", `{"v":1}`),
		mergeResource("aws_instance", "b", `{"v":1}`),
	)
	// Proposed deletes b.
	proposed := mergeStateOf("L1", 11,
		mergeResource("aws_instance", "a", `{"v":1}`),
	)

	merged, err := MergeStates(base, trunk, proposed)
	if err != nil {
		t.Fatalf("MergeStates: unexpected error %v", err)
	}
	if got, want := addressesOf(merged), []string{"aws_instance.a"}; !sliceEqual(got, want) {
		t.Errorf("merged addresses = %v, want %v", got, want)
	}
}

// TestMerge_RemovedByBoth_NotConflict: both operators delete X.
// Same address appears in both write sets, but the outcome
// (X deleted) is identical — we still flag it because tracking
// "did both decide X for the same reason" is impossible from
// just state diffs.
func TestMerge_RemovedByBoth_FlaggedConflict(t *testing.T) {
	base := mergeStateOf("L1", 10,
		mergeResource("aws_instance", "a", `{"v":1}`),
	)
	trunk := mergeStateOf("L1", 11)    // a removed by trunk
	proposed := mergeStateOf("L1", 11) // a also removed by proposed

	_, err := MergeStates(base, trunk, proposed)
	var conflict *WriteSetConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("MergeStates: want conflict, got %v", err)
	}
	if got, want := conflict.Addresses, []string{"aws_instance.a"}; !sliceEqual(got, want) {
		t.Errorf("conflict.Addresses = %v, want %v", got, want)
	}
}

// TestMerge_OutputConflict treats outputs as first-class addresses
// in the same namespace. Same name written on both sides → conflict.
func TestMerge_OutputConflict(t *testing.T) {
	base := mergeStateOf("L1", 10)
	base.Outputs = map[string]tfstate.Output{
		"k": {Value: json.RawMessage(`1`), Type: json.RawMessage(`"number"`)},
	}
	trunk := mergeStateOf("L1", 11)
	trunk.Outputs = map[string]tfstate.Output{
		"k": {Value: json.RawMessage(`2`), Type: json.RawMessage(`"number"`)},
	}
	proposed := mergeStateOf("L1", 11)
	proposed.Outputs = map[string]tfstate.Output{
		"k": {Value: json.RawMessage(`3`), Type: json.RawMessage(`"number"`)},
	}

	_, err := MergeStates(base, trunk, proposed)
	var conflict *WriteSetConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("MergeStates: want conflict, got %v", err)
	}
	if got, want := conflict.Addresses, []string{"output.k"}; !sliceEqual(got, want) {
		t.Errorf("conflict.Addresses = %v, want %v", got, want)
	}
}

// TestMerge_OutputDisjoint: trunk wrote output a, proposed wrote
// output b. Both should survive the merge.
func TestMerge_OutputDisjoint(t *testing.T) {
	base := mergeStateOf("L1", 10)
	trunk := mergeStateOf("L1", 11)
	trunk.Outputs = map[string]tfstate.Output{
		"a": {Value: json.RawMessage(`1`), Type: json.RawMessage(`"number"`)},
	}
	proposed := mergeStateOf("L1", 11)
	proposed.Outputs = map[string]tfstate.Output{
		"b": {Value: json.RawMessage(`2`), Type: json.RawMessage(`"number"`)},
	}

	merged, err := MergeStates(base, trunk, proposed)
	if err != nil {
		t.Fatalf("MergeStates: unexpected error %v", err)
	}
	if len(merged.Outputs) != 2 {
		t.Errorf("merged.Outputs has %d keys, want 2: %v", len(merged.Outputs), merged.Outputs)
	}
}

// TestMerge_LineageMismatch_AlwaysRejects: changing lineage means
// you're pushing a different state entirely. Never merge.
func TestMerge_LineageMismatch_AlwaysRejects(t *testing.T) {
	base := mergeStateOf("L1", 10)
	trunk := mergeStateOf("L1", 10)
	proposed := mergeStateOf("L2", 11)

	_, err := MergeStates(base, trunk, proposed)
	if !errors.Is(err, ErrLineageMismatch) {
		t.Errorf("MergeStates: want ErrLineageMismatch, got %v", err)
	}
}

// TestMerge_EmptyLineage_Tolerated covers backfill states that
// were never assigned a lineage. Empty side ≠ mismatch.
func TestMerge_EmptyLineage_Tolerated(t *testing.T) {
	base := mergeStateOf("", 10)
	trunk := mergeStateOf("", 10)
	proposed := mergeStateOf("L1", 11,
		mergeResource("aws_instance", "a", `{"v":1}`),
	)

	if _, err := MergeStates(base, trunk, proposed); err != nil {
		t.Errorf("MergeStates: unexpected error %v", err)
	}
}

// TestMerge_TrunkSerial_Bumped: merged serial is trunk+1, not
// proposed.Serial. Protects against a proposed state that
// happens to carry a serial collision-prone value.
func TestMerge_TrunkSerial_Bumped(t *testing.T) {
	base := mergeStateOf("L1", 10)
	trunk := mergeStateOf("L1", 42)
	proposed := mergeStateOf("L1", 11) // operator computed from base

	merged, err := MergeStates(base, trunk, proposed)
	if err != nil {
		t.Fatalf("MergeStates: unexpected error %v", err)
	}
	if got, want := merged.Serial, int64(43); got != want {
		t.Errorf("merged.Serial = %d, want %d", got, want)
	}
}

// TestResourceWriteSet_BytewiseInstances ensures the comparison
// catches changes inside the attribute blob even when the resource
// group structure is identical.
func TestResourceWriteSet_BytewiseInstances(t *testing.T) {
	from := mergeStateOf("L", 1,
		mergeResource("aws_instance", "a", `{"v":1}`),
		mergeResource("aws_instance", "b", `{"v":1}`),
	)
	to := mergeStateOf("L", 2,
		mergeResource("aws_instance", "a", `{"v":2}`), // changed
		mergeResource("aws_instance", "b", `{"v":1}`), // unchanged
	)

	fromMap, err := buildInstanceMap(from)
	if err != nil {
		t.Fatalf("buildInstanceMap(from): %v", err)
	}
	toMap, err := buildInstanceMap(to)
	if err != nil {
		t.Fatalf("buildInstanceMap(to): %v", err)
	}
	got, err := resourceWriteSet(fromMap, toMap)
	if err != nil {
		t.Fatalf("resourceWriteSet: %v", err)
	}
	if _, ok := got["aws_instance.a"]; !ok {
		t.Errorf("write set missing aws_instance.a; got %v", got)
	}
	if _, ok := got["aws_instance.b"]; ok {
		t.Errorf("write set unexpectedly contains aws_instance.b; got %v", got)
	}
}

func sliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
