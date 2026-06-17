package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
)

// jsonPathLeaf is one (path, before, after) row of a JSON path delta.
// "Leaf" here means a scalar — string / number / bool / null. Arrays
// of scalars are walked element-by-element; arrays of objects are
// walked recursively. The path is rendered Terraform-style:
//
//	root.tags.Environment
//	root.iam_policy.statement[0].effect
//
// We do NOT use jq-style ".tags[\"Environment\"]" because operators
// reading Terraform state expect Terraform paths.
//
// Sensitive controls redaction. When true, the renderer replaces
// Before/After with the literal "<sensitive>" — and a sensitivity
// TRANSITION (one side sensitive, the other not) is itself reported
// as a changed leaf so the operator notices the policy drift.
type jsonPathLeaf struct {
	Path      string
	Before    any
	After     any
	Status    string // "added" | "removed" | "changed"
	Sensitive bool
}

// pathSet is a small lookup structure built from terraform's
// `sensitive_paths` projection. The projection on disk is a JSON
// array of path-arrays, e.g.:
//
//	[["password"], ["nested", "key"]]
//
// pathSet stores those as joined-by-dot strings for cheap membership
// checks during the walk. We intentionally do NOT support [*]
// wildcards in sensitive_paths because terraform doesn't emit them at
// this layer.
type pathSet map[string]struct{}

// newPathSet parses a sensitive_paths jsonb blob into a pathSet.
// Tolerates nil/empty/non-array inputs — the caller has to handle
// pre-existing rows from before sensitive_paths existed in the
// schema, and a noisy parser would turn a normal diff into an error.
func newPathSet(raw json.RawMessage) pathSet {
	out := pathSet{}
	if len(raw) == 0 {
		return out
	}
	var parsed [][]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return out
	}
	for _, p := range parsed {
		parts := make([]string, 0, len(p))
		for _, seg := range p {
			parts = append(parts, fmt.Sprint(seg))
		}
		out[joinPath(parts)] = struct{}{}
	}
	return out
}

func (p pathSet) contains(path string) bool {
	_, ok := p[path]
	return ok
}

// diffJSON walks two arbitrary JSON values in parallel and produces
// the leaf-level delta. The result is sorted by path. Sensitive paths
// on EITHER side cause the value to be redacted on output.
//
// Implementation note: we operate on `any` after Unmarshal rather
// than the raw bytes, because RFC-8259 doesn't dictate key order but
// pgsql jsonb returns sorted keys; tests on the raw byte form would
// be flaky against any future canonicalization shift. The walk is
// recursive but state-bounded: max depth is the depth of the JSON
// input, which is itself bounded by terraform's own attribute schema.
func diffJSON(before, after json.RawMessage, sensitive pathSet) ([]jsonPathLeaf, error) {
	var bv, av any
	if len(before) > 0 {
		if err := json.Unmarshal(before, &bv); err != nil {
			return nil, fmt.Errorf("parse before attrs: %w", err)
		}
	}
	if len(after) > 0 {
		if err := json.Unmarshal(after, &av); err != nil {
			return nil, fmt.Errorf("parse after attrs: %w", err)
		}
	}
	var leaves []jsonPathLeaf
	walkDelta(nil, bv, av, sensitive, &leaves)
	sort.Slice(leaves, func(i, j int) bool { return leaves[i].Path < leaves[j].Path })
	return leaves, nil
}

// walkDelta is the recursive heart of diffJSON. The two values are
// "the value at the same path on each side". We classify by structural
// shape first (both objects, both arrays, both scalar, or mismatched)
// because that's what determines whether to recurse or to emit a leaf
// row. Mismatched-shape pairs are treated as a single "changed" leaf
// at the current path — descending further would produce noise.
func walkDelta(path []string, before, after any, sensitive pathSet, out *[]jsonPathLeaf) {
	switch {
	case before == nil && after == nil:
		return
	case before == nil:
		emitSubtree(path, after, sensitive, "added", out)
		return
	case after == nil:
		emitSubtree(path, before, sensitive, "removed", out)
		return
	}

	bObj, bIsObj := before.(map[string]any)
	aObj, aIsObj := after.(map[string]any)
	if bIsObj && aIsObj {
		keys := unionKeys(bObj, aObj)
		for _, k := range keys {
			walkDelta(append(path, k), bObj[k], aObj[k], sensitive, out)
		}
		return
	}

	bArr, bIsArr := before.([]any)
	aArr, aIsArr := after.([]any)
	if bIsArr && aIsArr {
		n := len(bArr)
		if len(aArr) > n {
			n = len(aArr)
		}
		for i := 0; i < n; i++ {
			var bv, av any
			if i < len(bArr) {
				bv = bArr[i]
			}
			if i < len(aArr) {
				av = aArr[i]
			}
			walkDelta(append(path, "["+strconv.Itoa(i)+"]"), bv, av, sensitive, out)
		}
		return
	}

	// Mismatched shapes OR both scalars OR mixed scalar/composite.
	// We never recurse on a shape mismatch — emit one "changed" row
	// at this path instead so the operator sees one signal, not a
	// cascade of misleading sub-paths.
	if !scalarEqual(before, after) {
		p := joinPath(path)
		*out = append(*out, jsonPathLeaf{
			Path:      p,
			Before:    before,
			After:     after,
			Status:    "changed",
			Sensitive: sensitive.contains(p),
		})
	}
}

// emitSubtree turns a single-sided subtree into a sequence of leaf
// rows with the given status. Used for the added / removed branches
// at the top level. Composite values produce one row per leaf so the
// operator can see the SHAPE of what's appearing/disappearing rather
// than a single "<object>" black-box row.
func emitSubtree(path []string, v any, sensitive pathSet, status string, out *[]jsonPathLeaf) {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			emitSubtree(append(path, k), t[k], sensitive, status, out)
		}
	case []any:
		for i, elem := range t {
			emitSubtree(append(path, "["+strconv.Itoa(i)+"]"), elem, sensitive, status, out)
		}
	default:
		p := joinPath(path)
		row := jsonPathLeaf{
			Path:      p,
			Status:    status,
			Sensitive: sensitive.contains(p),
		}
		switch status {
		case "added":
			row.After = v
		case "removed":
			row.Before = v
		}
		*out = append(*out, row)
	}
}

// scalarEqual is the leaf-equality predicate for the walker. We
// avoid reflect.DeepEqual because (a) it's slow, and (b) the only
// pairs we ever see at this layer are scalars or scalar-vs-composite
// mismatches; a focused implementation is both faster and easier to
// reason about. The marshal-and-compare-bytes fallback handles
// numeric edge cases (1.0 vs 1) consistently with how postgres jsonb
// serializes them.
func scalarEqual(a, b any) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	switch av := a.(type) {
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	case float64:
		bv, ok := b.(float64)
		return ok && av == bv
	}
	ba, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return bytes.Equal(ba, bb)
}

// unionKeys returns the sorted union of the keys of two objects.
// Sort makes the diff stable across runs; without sort, a Go map
// iteration order would scramble the leaf ordering of object diffs
// despite the top-level sort, because path strings sort lexically
// but the walk visits keys in map order. (We later re-sort, but the
// walk order also dictates how arrays-inside-objects are paired —
// stable here is friendlier to mental review of the source.)
func unionKeys(a, b map[string]any) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	for k := range a {
		seen[k] = struct{}{}
	}
	for k := range b {
		seen[k] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// joinPath renders a path stack as a dotted string. Index segments
// (already wrapped in brackets by walkDelta) are appended without an
// extra dot so the output reads like "tags.foo[0].bar" rather than
// "tags.foo.[0].bar".
func joinPath(parts []string) string {
	if len(parts) == 0 {
		return "root"
	}
	var buf bytes.Buffer
	buf.WriteString("root")
	for _, p := range parts {
		if len(p) > 0 && p[0] == '[' {
			buf.WriteString(p)
			continue
		}
		buf.WriteByte('.')
		buf.WriteString(p)
	}
	return buf.String()
}

// renderValue formats a scalar for the diff output. Sensitive values
// are masked. Strings are quoted to disambiguate from numbers; bools
// and numbers and null use their JSON spelling.
func renderValue(v any, sensitive bool) string {
	if sensitive {
		return "<sensitive>"
	}
	if v == nil {
		return "null"
	}
	switch t := v.(type) {
	case string:
		// Use json marshal to escape quotes/control chars. Avoids
		// hand-rolling the escape rules.
		b, _ := json.Marshal(t)
		return string(b)
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'g', -1, 64)
	}
	b, _ := json.Marshal(v)
	return string(b)
}
