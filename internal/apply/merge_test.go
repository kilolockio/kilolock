package apply

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/kilolockio/kilolock/internal/slice"
)

// makeTrunk constructs a TrunkState with the given (address ->
// rawInstance JSON) pairs at a fixed lineage/serial.
//
// Addresses are parsed for module/data prefixes so the tests can
// be specified by canonical-address strings. This keeps each test
// case readable in one expression.
func makeTrunk(t *testing.T, lineage string, serial int64, rows map[string]string) *slice.TrunkState {
	t.Helper()
	out := &slice.TrunkState{
		Version:          4,
		TerraformVersion: "1.5.7",
		Serial:           serial,
		Lineage:          lineage,
	}
	for addr, instJSON := range rows {
		mode := "managed"
		rest := addr
		if r, ok := stripPrefix(rest, "data."); ok {
			mode = "data"
			rest = r
		}
		var module string
		if before, after, ok := splitAtModuleBoundary(rest); ok {
			module = before
			rest = after
		}
		ty, name, ok := splitTypeName(rest)
		if !ok {
			t.Fatalf("makeTrunk: cannot parse address %q", addr)
		}
		out.Resources = append(out.Resources, slice.TrunkResource{
			Module:    module,
			Mode:      mode,
			Type:      ty,
			Name:      name,
			Provider:  `provider["registry.terraform.io/hashicorp/null"]`,
			Instances: json.RawMessage(instJSON),
		})
	}
	return out
}

// splitAtModuleBoundary returns ("module.web", "aws_instance.x", true)
// for "module.web.aws_instance.x". For nested modules
// (module.a.module.b.aws_instance.x), the entire chain up to but
// excluding the last (type.name) pair is the module path.
func splitAtModuleBoundary(addr string) (modulePart, rest string, ok bool) {
	if len(addr) < len("module.") || addr[:len("module.")] != "module." {
		return "", addr, false
	}
	// Walk forward, tracking the LAST module boundary. Module
	// paths look like module.X[.module.Y]*; the rest is type.name.
	// We split at the last "module.X" segment that the type.name
	// trail follows. For simplicity here (tests-only helper) we
	// accept addresses with no module-key indexing.
	lastModule := 0
	for i := 0; i+len("module.") <= len(addr); i++ {
		if addr[i:i+len("module.")] == "module." {
			lastModule = i
		}
	}
	// After lastModule we have "module.X.<type>.<name>".
	// Find the dot after the X and split there.
	tail := addr[lastModule+len("module."):]
	dot := -1
	for i, c := range tail {
		if c == '.' {
			dot = i
			break
		}
	}
	if dot < 0 {
		return "", addr, false
	}
	modulePart = addr[:lastModule+len("module.")+dot]
	rest = addr[lastModule+len("module.")+dot+1:]
	return modulePart, rest, true
}

func splitTypeName(s string) (ty, name string, ok bool) {
	for i, c := range s {
		if c == '.' {
			return s[:i], s[i+1:], true
		}
	}
	return "", "", false
}

func stripPrefix(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && s[:len(prefix)] == prefix {
		return s[len(prefix):], true
	}
	return s, false
}

// TestBuildMergedState_UpdateReplacesOnlyWriteSet is the row-level
// commit's headline invariant: a write_set member's row in the
// merged state is taken from post-apply, and a NON-write-set row
// is taken from trunk verbatim. Verifies via the per-address
// instances payload which is what would diverge if the merge ever
// pulled from the wrong source.
func TestBuildMergedState_UpdateReplacesOnlyWriteSet(t *testing.T) {
	trunk := makeTrunk(t, "lin-a", 10, map[string]string{
		"null_resource.a": `[{"attributes":{"v":1}}]`,
		"null_resource.b": `[{"attributes":{"v":2}}]`,
		"null_resource.c": `[{"attributes":{"v":3}}]`,
	})
	postApply := makeTrunk(t, "lin-a", 10, map[string]string{
		"null_resource.a": `[{"attributes":{"v":99}}]`, // mutated
		"null_resource.b": `[{"attributes":{"v":2}}]`,
		"null_resource.c": `[{"attributes":{"v":3}}]`,
	})
	writeSet := []string{"null_resource.a"}
	footprint := map[string]struct{}{
		"null_resource.a": {},
		"null_resource.b": {},
		"null_resource.c": {},
	}

	out, err := buildMergedState(trunk, postApply, writeSet, footprint)
	if err != nil {
		t.Fatalf("buildMergedState: %v", err)
	}
	if out.NewSerial != 11 {
		t.Errorf("new serial: got %d want 11", out.NewSerial)
	}
	if !reflect.DeepEqual(out.AppliedAddresses, []string{"null_resource.a"}) {
		t.Errorf("applied addresses: got %v want [null_resource.a]", out.AppliedAddresses)
	}

	parsed, err := slice.ParseTrunkState(out.MergedBytes)
	if err != nil {
		t.Fatalf("parse merged bytes: %v", err)
	}
	got := map[string]string{}
	for _, r := range parsed.Resources {
		got[r.Address()] = canonicalJSON(t, r.Instances)
	}
	wantA := canonicalJSON(t, []byte(`[{"attributes":{"v":99}}]`))
	if got["null_resource.a"] != wantA {
		t.Errorf("write_set member: got %s want %s", got["null_resource.a"], wantA)
	}
	wantB := canonicalJSON(t, []byte(`[{"attributes":{"v":2}}]`))
	if got["null_resource.b"] != wantB {
		t.Errorf("non-write_set member must be unchanged trunk row: got %s want %s",
			got["null_resource.b"], wantB)
	}
}

// canonicalJSON re-encodes the input through Unmarshal/Marshal so
// indent and key order are normalized. Used by the assertions
// above so MarshalTrunkState's choice to indent inner RawMessages
// doesn't leak into the test expectations.
func canonicalJSON(t *testing.T, b []byte) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("canonicalize: %v\ninput=%s", err, string(b))
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	return string(out)
}

// TestBuildMergedState_DeleteRemovesFromTrunk verifies that an
// address present in write_set but absent from post-apply is
// dropped from the merged state (i.e. terraform deleted it).
func TestBuildMergedState_DeleteRemovesFromTrunk(t *testing.T) {
	trunk := makeTrunk(t, "lin", 1, map[string]string{
		"null_resource.a": `[{"attributes":{"v":1}}]`,
		"null_resource.b": `[{"attributes":{"v":2}}]`,
	})
	postApply := makeTrunk(t, "lin", 1, map[string]string{
		"null_resource.b": `[{"attributes":{"v":2}}]`,
	})
	out, err := buildMergedState(trunk, postApply,
		[]string{"null_resource.a"},
		map[string]struct{}{"null_resource.a": {}, "null_resource.b": {}},
	)
	if err != nil {
		t.Fatalf("buildMergedState: %v", err)
	}
	parsed, _ := slice.ParseTrunkState(out.MergedBytes)
	if len(parsed.Resources) != 1 || parsed.Resources[0].Address() != "null_resource.b" {
		t.Fatalf("expected only null_resource.b in merged state, got %+v", parsed.Resources)
	}
}

// TestBuildMergedState_CreateInsertsFromPostApply: address is in
// write_set, post-apply has it, trunk doesn't → it's a create and
// the merged state must include the post-apply row.
func TestBuildMergedState_CreateInsertsFromPostApply(t *testing.T) {
	trunk := makeTrunk(t, "lin", 1, map[string]string{
		"null_resource.a": `[{"attributes":{"v":1}}]`,
	})
	postApply := makeTrunk(t, "lin", 1, map[string]string{
		"null_resource.a": `[{"attributes":{"v":1}}]`,
		"null_resource.b": `[{"attributes":{"v":2}}]`,
	})
	out, err := buildMergedState(trunk, postApply,
		[]string{"null_resource.b"},
		map[string]struct{}{"null_resource.a": {}, "null_resource.b": {}},
	)
	if err != nil {
		t.Fatalf("buildMergedState: %v", err)
	}
	parsed, _ := slice.ParseTrunkState(out.MergedBytes)
	got := map[string]bool{}
	for _, r := range parsed.Resources {
		got[r.Address()] = true
	}
	if !got["null_resource.a"] {
		t.Errorf("merged state missing null_resource.a")
	}
	if !got["null_resource.b"] {
		t.Errorf("merged state missing null_resource.b")
	}
}

// TestBuildMergedState_SurpriseResourceRejected verifies the v2
// safety invariant: a post-apply state with a resource outside
// the HCL footprint is rejected before any merge happens. The
// orchestrator has no reservation for surprise addresses, so
// committing them would silently overwrite anything a parallel
// writer is doing there.
func TestBuildMergedState_SurpriseResourceRejected(t *testing.T) {
	trunk := makeTrunk(t, "lin", 1, map[string]string{
		"null_resource.a": `[{"attributes":{"v":1}}]`,
	})
	postApply := makeTrunk(t, "lin", 1, map[string]string{
		"null_resource.a":        `[{"attributes":{"v":2}}]`,
		"null_resource.surprise": `[{"attributes":{"v":99}}]`,
	})
	footprint := map[string]struct{}{"null_resource.a": {}}
	_, err := buildMergedState(trunk, postApply,
		[]string{"null_resource.a"}, footprint,
	)
	if err == nil {
		t.Fatal("expected surprise-resource rejection, got nil")
	}
}

// TestBuildMergedState_NoChangePreservesSerialAdvance: even when
// the operator's plan was all no-ops (write_set empty), an apply
// still produces a new state version with serial+1 (the audit row
// records that the apply happened). This matches Terraform's own
// behavior: a no-op apply still bumps the serial.
func TestBuildMergedState_NoChangePreservesSerialAdvance(t *testing.T) {
	trunk := makeTrunk(t, "lin", 7, map[string]string{
		"null_resource.a": `[{"attributes":{"v":1}}]`,
	})
	postApply := makeTrunk(t, "lin", 7, map[string]string{
		"null_resource.a": `[{"attributes":{"v":1}}]`,
	})
	out, err := buildMergedState(trunk, postApply,
		nil,
		map[string]struct{}{"null_resource.a": {}},
	)
	if err != nil {
		t.Fatalf("buildMergedState: %v", err)
	}
	if out.NewSerial != 8 {
		t.Errorf("serial advance: got %d want 8", out.NewSerial)
	}
	if len(out.AppliedAddresses) != 0 {
		t.Errorf("expected no applied addresses, got %v", out.AppliedAddresses)
	}
}

// TestBuildMergedState_NonWriteSetRowIsByteIdentical is the
// row-level commit's correctness regression test. The merged
// state document handed to WriteState carries the trunk's
// per-resource attribute bytes for any address outside the
// write_set, and those bytes MUST be byte-identical so v1's
// attributes_hash matches and applyResourceDelta sees a no-op.
//
// A naive re-serialization that adds indent (or otherwise
// rewrites whitespace) causes every non-write-set row to be
// close+reinserted at the apply, defeating the entire point of
// v2c. This test pins compact marshal behavior and prevents
// regressions when the slice marshal path is touched.
func TestBuildMergedState_NonWriteSetRowIsByteIdentical(t *testing.T) {
	// Inner attribute bytes mimic Postgres-canonical JSONB: sorted
	// keys, compact whitespace. Real trunks read out of Postgres
	// look exactly like this.
	const trunkInstancesB = `[{"attributes":{"a":1,"b":"two","c":[1,2,3]}}]`
	trunk := makeTrunk(t, "lin", 5, map[string]string{
		"null_resource.a": `[{"attributes":{"v":1}}]`,
		"null_resource.b": trunkInstancesB,
	})
	postApply := makeTrunk(t, "lin", 5, map[string]string{
		"null_resource.a": `[{"attributes":{"v":99}}]`,
		"null_resource.b": trunkInstancesB,
	})
	out, err := buildMergedState(trunk, postApply,
		[]string{"null_resource.a"},
		map[string]struct{}{"null_resource.a": {}, "null_resource.b": {}},
	)
	if err != nil {
		t.Fatalf("buildMergedState: %v", err)
	}
	parsed, err := slice.ParseTrunkState(out.MergedBytes)
	if err != nil {
		t.Fatalf("parse merged: %v", err)
	}
	for _, r := range parsed.Resources {
		if r.Address() != "null_resource.b" {
			continue
		}
		gotBytes := string(r.Instances)
		if gotBytes != trunkInstancesB {
			t.Errorf(
				"non-write_set row instances must be byte-identical to trunk to keep attributes_hash stable\n  trunk : %s\n  merged: %s",
				trunkInstancesB, gotBytes,
			)
		}
	}
}

func TestValidatePostApplyHasNoSurprises_SortedOutput(t *testing.T) {
	postApply := makeTrunk(t, "lin", 1, map[string]string{
		"null_resource.a": `[]`,
		"null_resource.z": `[]`,
		"null_resource.m": `[]`,
	})
	footprint := map[string]struct{}{"null_resource.m": {}}
	bad := validatePostApplyHasNoSurprises(postApply, footprint)
	want := []string{"null_resource.a", "null_resource.z"}
	if !reflect.DeepEqual(bad, want) {
		t.Errorf("surprises: got %v want %v", bad, want)
	}
}

func TestJSONRawEqual_KeyReorderingMatches(t *testing.T) {
	a := json.RawMessage(`{"x":1,"y":2}`)
	b := json.RawMessage(`{"y":2,"x":1}`)
	eq, err := jsonRawEqual(a, b)
	if err != nil {
		t.Fatalf("jsonRawEqual: %v", err)
	}
	if !eq {
		t.Errorf("expected key-reordered JSON to be equal")
	}
}

func TestJSONRawEqual_RealDifferenceDetected(t *testing.T) {
	a := json.RawMessage(`{"x":1}`)
	b := json.RawMessage(`{"x":2}`)
	eq, err := jsonRawEqual(a, b)
	if err != nil {
		t.Fatalf("jsonRawEqual: %v", err)
	}
	if eq {
		t.Errorf("expected differing JSON to be reported unequal")
	}
}
