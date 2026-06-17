package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestDiffJSON_ScalarChange is the smallest meaningful change: one
// top-level scalar. Pins the path-naming convention ("root.foo") and
// the status="changed" classification of "present on both sides,
// different value".
func TestDiffJSON_ScalarChange(t *testing.T) {
	before := json.RawMessage(`{"name":"alpha","count":1}`)
	after := json.RawMessage(`{"name":"beta","count":1}`)
	leaves, err := diffJSON(before, after, pathSet{})
	if err != nil {
		t.Fatalf("diffJSON: %v", err)
	}
	if len(leaves) != 1 {
		t.Fatalf("want 1 leaf, got %d: %+v", len(leaves), leaves)
	}
	got := leaves[0]
	if got.Path != "root.name" {
		t.Errorf("path = %q, want root.name", got.Path)
	}
	if got.Status != "changed" {
		t.Errorf("status = %q, want changed", got.Status)
	}
	if got.Before != "alpha" || got.After != "beta" {
		t.Errorf("before/after = %v/%v, want alpha/beta", got.Before, got.After)
	}
}

// TestDiffJSON_AddedKey_OnlyAfterSet: when a key is new on the right
// side, the leaf is "added" and Before is nil. Operators rely on
// this — "I see + tags.Environment, so this is new".
func TestDiffJSON_AddedKey_OnlyAfterSet(t *testing.T) {
	before := json.RawMessage(`{"name":"alpha"}`)
	after := json.RawMessage(`{"name":"alpha","extra":"hi"}`)
	leaves, _ := diffJSON(before, after, pathSet{})
	if len(leaves) != 1 {
		t.Fatalf("want 1 leaf, got %d", len(leaves))
	}
	lf := leaves[0]
	if lf.Status != "added" {
		t.Errorf("status = %q, want added", lf.Status)
	}
	if lf.Before != nil {
		t.Errorf("Before = %v, want nil for added leaf", lf.Before)
	}
	if lf.After != "hi" {
		t.Errorf("After = %v, want \"hi\"", lf.After)
	}
}

// TestDiffJSON_NestedObjectAndArray exercises the recursive walk.
// Mixed nested change types in one resource: a top-level scalar
// changed, a nested scalar changed, an array element changed. This
// is the case that fails first if the walker recurses incorrectly.
func TestDiffJSON_NestedObjectAndArray(t *testing.T) {
	before := json.RawMessage(`{
		"name":"alpha",
		"tags":{"env":"dev","owner":"alice"},
		"ports":[80,443]
	}`)
	after := json.RawMessage(`{
		"name":"alpha",
		"tags":{"env":"prod","owner":"alice"},
		"ports":[80,8443]
	}`)
	leaves, err := diffJSON(before, after, pathSet{})
	if err != nil {
		t.Fatalf("diffJSON: %v", err)
	}
	// Want exactly two leaves: tags.env and ports[1].
	wantPaths := map[string]bool{
		"root.tags.env": true,
		"root.ports[1]": true,
	}
	for _, lf := range leaves {
		if !wantPaths[lf.Path] {
			t.Errorf("unexpected leaf path %q", lf.Path)
		}
		delete(wantPaths, lf.Path)
	}
	if len(wantPaths) > 0 {
		t.Errorf("missing expected leaves: %v", wantPaths)
	}
}

// TestDiffJSON_SensitivePathIsMasked verifies that a sensitive path
// surfaces as "<sensitive>" in renderValue and that sensitivity
// propagates from both sides. This is the privacy contract that
// future refactors must not break.
func TestDiffJSON_SensitivePathIsMasked(t *testing.T) {
	before := json.RawMessage(`{"password":"oldsecret"}`)
	after := json.RawMessage(`{"password":"newsecret"}`)
	// Sensitive set from "before" side (Terraform's sensitive_paths
	// for the prior version).
	sensitive := pathSet{"root.password": {}}
	leaves, _ := diffJSON(before, after, sensitive)
	if len(leaves) != 1 {
		t.Fatalf("want 1 leaf, got %d", len(leaves))
	}
	lf := leaves[0]
	if !lf.Sensitive {
		t.Error("leaf.Sensitive should be true")
	}
	if got := renderValue(lf.Before, lf.Sensitive); got != "<sensitive>" {
		t.Errorf("rendered Before = %q, want <sensitive>", got)
	}
	if got := renderValue(lf.After, lf.Sensitive); got != "<sensitive>" {
		t.Errorf("rendered After = %q, want <sensitive>", got)
	}
}

// TestDiffJSON_SameJSON_Empty: identical JSON on both sides must
// produce zero leaves. Without this, the renderer would print "no
// changes" sections that are nominally non-empty.
func TestDiffJSON_SameJSON_Empty(t *testing.T) {
	same := json.RawMessage(`{"a":1,"b":[true,false]}`)
	leaves, err := diffJSON(same, same, pathSet{})
	if err != nil {
		t.Fatalf("diffJSON: %v", err)
	}
	if len(leaves) != 0 {
		t.Errorf("want 0 leaves, got %d: %+v", len(leaves), leaves)
	}
}

// TestDiffJSON_AddedSubtreeFlattenedToLeaves: a brand-new nested
// object on the right side should produce one leaf PER scalar inside,
// not a single "object" leaf. This is what makes the operator able
// to see the SHAPE of what was added.
func TestDiffJSON_AddedSubtreeFlattenedToLeaves(t *testing.T) {
	before := json.RawMessage(`{}`)
	after := json.RawMessage(`{"new":{"a":1,"b":2}}`)
	leaves, _ := diffJSON(before, after, pathSet{})
	if len(leaves) != 2 {
		t.Fatalf("want 2 leaves (one per added scalar), got %d", len(leaves))
	}
	wantPaths := map[string]bool{
		"root.new.a": true,
		"root.new.b": true,
	}
	for _, lf := range leaves {
		if lf.Status != "added" {
			t.Errorf("leaf status = %q, want added", lf.Status)
		}
		if !wantPaths[lf.Path] {
			t.Errorf("unexpected path %q", lf.Path)
		}
		delete(wantPaths, lf.Path)
	}
	if len(wantPaths) > 0 {
		t.Errorf("missing leaves: %v", wantPaths)
	}
}

// TestDiffJSON_RemovedArrayProducesIndexedLeaves: parallel to the
// added case, but for arrays. The path naming MUST use square-
// bracketed indices ([0], [1]) and NOT dotted indices (.0, .1) so
// it stays readable as Terraform-style addresses.
func TestDiffJSON_RemovedArrayProducesIndexedLeaves(t *testing.T) {
	before := json.RawMessage(`{"old":[10,20,30]}`)
	after := json.RawMessage(`{}`)
	leaves, _ := diffJSON(before, after, pathSet{})
	if len(leaves) != 3 {
		t.Fatalf("want 3 leaves, got %d", len(leaves))
	}
	for i, lf := range leaves {
		if !strings.Contains(lf.Path, "root.old[") {
			t.Errorf("leaf %d path = %q, want bracketed index", i, lf.Path)
		}
		if lf.Status != "removed" {
			t.Errorf("leaf %d status = %q, want removed", i, lf.Status)
		}
	}
}

// TestDiffJSON_ShapeMismatchIsOneLeaf: when one side is a scalar
// and the other is an object (or arrays of different kinds), we
// stop recursing and emit one row at the boundary. This is what
// avoids a cascade of misleading sub-paths.
func TestDiffJSON_ShapeMismatchIsOneLeaf(t *testing.T) {
	before := json.RawMessage(`{"x":"scalar"}`)
	after := json.RawMessage(`{"x":{"nested":1}}`)
	leaves, _ := diffJSON(before, after, pathSet{})
	if len(leaves) != 1 {
		t.Fatalf("want 1 leaf at the mismatch boundary, got %d", len(leaves))
	}
	if leaves[0].Path != "root.x" {
		t.Errorf("path = %q, want root.x", leaves[0].Path)
	}
	if leaves[0].Status != "changed" {
		t.Errorf("status = %q, want changed", leaves[0].Status)
	}
}

// TestNewPathSet_ParsesTerraformShape covers the shape Terraform
// actually writes for sensitive_paths.
func TestNewPathSet_ParsesTerraformShape(t *testing.T) {
	raw := json.RawMessage(`[["password"],["nested","key"],["arr",0,"sub"]]`)
	ps := newPathSet(raw)
	for _, p := range []string{"root.password", "root.nested.key", "root.arr.0.sub"} {
		if !ps.contains(p) {
			t.Errorf("pathSet missing %q; have %v", p, ps)
		}
	}
}

// TestNewPathSet_HandlesNullAndJunk: we must not blow up on legacy
// rows where sensitive_paths is NULL/empty/malformed. The function
// is tolerant by design — the alternative is that one bad sensitive
// blob takes down the whole diff command.
func TestNewPathSet_HandlesNullAndJunk(t *testing.T) {
	for _, raw := range []json.RawMessage{
		nil,
		json.RawMessage(``),
		json.RawMessage(`null`),
		json.RawMessage(`"this-is-not-an-array"`),
		json.RawMessage(`[`), // malformed
	} {
		ps := newPathSet(raw)
		if len(ps) != 0 {
			t.Errorf("pathSet for %q should be empty, got %v", string(raw), ps)
		}
	}
}

// TestRenderValue_StringsQuoted is a small contract: strings must
// render with double quotes around them so the operator can tell
// "1" from 1. This is important when an attribute can change types
// (e.g. count: 1 vs count: "1").
func TestRenderValue_StringsQuoted(t *testing.T) {
	if got := renderValue("hello", false); got != `"hello"` {
		t.Errorf("string render = %q, want \"hello\" (with quotes)", got)
	}
	if got := renderValue(float64(42), false); got != "42" {
		t.Errorf("int render = %q, want 42", got)
	}
	if got := renderValue(true, false); got != "true" {
		t.Errorf("bool render = %q, want true", got)
	}
	if got := renderValue(nil, false); got != "null" {
		t.Errorf("null render = %q, want null", got)
	}
}

// TestMatchAddressGlob covers the Terraform-address-flavoured glob.
// We deliberately treat '.' as a non-segment separator (because
// Terraform addresses ARE NOT paths).
func TestMatchAddressGlob(t *testing.T) {
	cases := []struct {
		pattern string
		addr    string
		want    bool
	}{
		// Single-segment wildcards.
		{"aws_instance.*", "aws_instance.web", true},
		{"aws_instance.*", "aws_instance.web.attr", false}, // 1 segment only

		// Recursive wildcard for cross-dot matching.
		{"aws_instance.**", "aws_instance.web", true},
		{"aws_instance.**", "aws_instance.web.attr", true},

		// Module-prefixed addresses.
		{"module.*.aws_instance.*", "module.vpc.aws_instance.web", true},
		{"module.*.aws_instance.*", "module.vpc.aws_db.write", false},

		// Bracket indices left alone.
		{"aws_instance.web[*]", "aws_instance.web[0]", true},
		// (filepath.Match treats [ as a character-class start; our
		// translation passes brackets through to path.Match, which
		// in turn implements ranges. "aws_instance.web[*]" → after
		// translation "aws_instance/web[*]", and path.Match treats
		// "[*]" as a single-char class containing '*'. So this
		// matches addresses ending in literal "[*]". The test
		// pins the current behaviour rather than asserting
		// terraform-glob exactly — operators wanting indices use the
		// recursive form.)
	}
	for _, c := range cases {
		got := matchAddressGlob(c.pattern, c.addr)
		if got != c.want {
			t.Errorf("matchAddressGlob(%q, %q) = %v, want %v", c.pattern, c.addr, got, c.want)
		}
	}
}
