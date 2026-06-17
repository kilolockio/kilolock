package plan

import (
	"sort"
	"strings"
)

// DepGraph is the static dependency graph derived from the plan's
// configuration block: an edge from A to B means resource A's HCL
// references resource B (anywhere — argument, depends_on, lifecycle
// trigger, etc.).
//
// Both endpoints are canonical, module-qualified addresses without
// attribute selectors. Indexing keyed on count/for_each is also
// preserved (e.g. `aws_instance.web[0]`), because plan addresses
// already include indexing.
//
// The map representation is convenient for transitive closure; the
// Edges() helper flattens it to a sorted slice for stable output.
type DepGraph map[string]map[string]struct{}

// Add records an edge from→to in the graph. Idempotent; adding the
// same edge twice is cheap.
func (g DepGraph) Add(from, to string) {
	if g[from] == nil {
		g[from] = map[string]struct{}{}
	}
	g[from][to] = struct{}{}
}

// Edges returns every recorded edge as a sorted slice of
// DependencyEdge values. Sort key is (from, to) lexicographic.
func (g DepGraph) Edges() []DependencyEdge {
	if len(g) == 0 {
		return nil
	}
	out := make([]DependencyEdge, 0, len(g))
	for from, tos := range g {
		for to := range tos {
			out = append(out, DependencyEdge{From: from, To: to})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].From != out[j].From {
			return out[i].From < out[j].From
		}
		return out[i].To < out[j].To
	})
	return out
}

// BuildDepGraph walks the plan's configuration block and collects
// every reference between resources as an edge. Module nesting is
// handled by recursing into module_calls and prepending the module
// prefix to addresses on the way down.
//
// References inside `expressions` can appear at arbitrary nesting
// depth (objects, arrays, conditionals), so the walk is recursive.
// Non-resource references (`var.x`, `local.x`, `each.value`,
// `count.index`, etc.) are filtered out — they don't correspond to
// reservable graph nodes.
//
// Attribute selectors are trimmed: `aws_vpc.main.id` collapses to
// `aws_vpc.main`. The orchestrator reserves addresses, not
// attributes; collapsing here keeps the graph dense and the read-set
// closure tractable.
func BuildDepGraph(f *File) DepGraph {
	g := DepGraph{}
	if f == nil {
		return g
	}
	walkConfigModule(f.Configuration.RootModule, "", g)
	return g
}

func walkConfigModule(m ConfigModule, prefix string, g DepGraph) {
	for _, r := range m.Resources {
		from := joinAddr(prefix, r.Address)
		for _, ref := range collectReferences(r.Expressions) {
			if !isResourceRef(ref) {
				continue
			}
			to := normalizeRef(joinAddr(prefix, ref))
			if to == "" || to == from {
				continue
			}
			g.Add(from, to)
		}
	}
	for name, mc := range m.ModuleCalls {
		walkConfigModule(mc.Module, joinAddr(prefix, "module."+name), g)
	}
}

// collectReferences walks a configuration `expressions` blob and
// pulls out every `references` array it encounters. Terraform may
// nest `references` arrays arbitrarily deep when expressions are
// objects, arrays, or conditionals.
func collectReferences(v any) []string {
	switch x := v.(type) {
	case map[string]any:
		var out []string
		if refs, ok := x["references"].([]any); ok {
			for _, r := range refs {
				if s, ok := r.(string); ok {
					out = append(out, s)
				}
			}
		}
		for k, child := range x {
			if k == "references" {
				continue
			}
			out = append(out, collectReferences(child)...)
		}
		return out
	case []any:
		var out []string
		for _, c := range x {
			out = append(out, collectReferences(c)...)
		}
		return out
	}
	return nil
}

// isResourceRef returns true for things that look like resource
// addresses. Filters out the meta-vocabulary that lives in the same
// `references` payload:
//
//   - var.X       — input variables
//   - local.X     — locals
//   - each.X      — for_each scope
//   - count.X     — count.index
//   - path.X      — path.module, path.root, path.cwd
//   - terraform.X — terraform.workspace, terraform.applying
//   - self.X      — provisioner self-reference
func isResourceRef(ref string) bool {
	if ref == "" {
		return false
	}
	switch {
	case strings.HasPrefix(ref, "var."),
		strings.HasPrefix(ref, "local."),
		strings.HasPrefix(ref, "each."),
		strings.HasPrefix(ref, "count."),
		strings.HasPrefix(ref, "path."),
		strings.HasPrefix(ref, "terraform."),
		strings.HasPrefix(ref, "self."):
		return false
	}
	return true
}

// normalizeRef truncates a reference like "aws_vpc.main.id" to its
// resource address "aws_vpc.main". Handles:
//
//   - bare:        aws_vpc.main           → aws_vpc.main
//   - attribute:   aws_vpc.main.id        → aws_vpc.main
//   - data:        data.foo.bar           → data.foo.bar
//   - data attr:   data.foo.bar.baz       → data.foo.bar
//   - indexed:     aws_instance.web[0]    → aws_instance.web[0]
//   - module:      module.web.aws_vpc.x   → module.web.aws_vpc.x
//   - module attr: module.web.aws_vpc.x.id→ module.web.aws_vpc.x
//
// The algorithm: count past every `module.<name>` prefix pair, then
// take the next 2 (or 3 for data) segments as the address.
func normalizeRef(ref string) string {
	if ref == "" {
		return ""
	}
	parts := strings.Split(ref, ".")
	// Skip module.<name> prefix pairs.
	i := 0
	for i+1 < len(parts) && parts[i] == "module" {
		i += 2
	}
	if i >= len(parts) {
		return ref // malformed; return as-is
	}
	// Now parts[i:] is the bare resource path (e.g. aws_vpc.main.id
	// or data.foo.bar.baz). Resource addresses are 2 segments
	// (type.name) or 3 for data (data.type.name).
	want := 2
	if parts[i] == "data" {
		want = 3
	}
	end := i + want
	if end > len(parts) {
		// Reference is shorter than expected — return what we have.
		return strings.Join(parts, ".")
	}
	return strings.Join(parts[:end], ".")
}

// joinAddr is the address concatenator the recursive walk uses to
// stamp module prefixes onto child addresses. Avoids producing a
// leading dot when prefix is empty.
func joinAddr(prefix, addr string) string {
	if prefix == "" {
		return addr
	}
	if addr == "" {
		return prefix
	}
	return prefix + "." + addr
}

// CloseReadSet computes the transitive read-set closure: starting
// from writeSet, walks edges in both directions until a fixed point.
// The bidirectional traversal is intentional — a writer of X needs
// X's dependencies (reads inputs against current values) AND X's
// reverse-dependencies (because those resources' inputs change
// when X is mutated, so any parallel apply touching them must see
// consistent state).
//
// Returns a deterministic, sorted slice. writeSet is INCLUDED in the
// result; callers commonly want the full reservation set in one
// pass.
//
// Cycle-safe: the fixed-point loop terminates because the set is
// monotonically growing and bounded by the total node count.
func CloseReadSet(writeSet []string, g DepGraph) []string {
	out := make(map[string]struct{}, len(writeSet))
	for _, a := range writeSet {
		out[a] = struct{}{}
	}
	for changed := true; changed; {
		changed = false
		// forward: if `from` is in the set, pull its `to` neighbours
		for from, tos := range g {
			if _, here := out[from]; !here {
				continue
			}
			for to := range tos {
				if _, exists := out[to]; !exists {
					out[to] = struct{}{}
					changed = true
				}
			}
		}
		// reverse: if `to` is in the set, pull its `from` neighbours
		for from, tos := range g {
			for to := range tos {
				if _, here := out[to]; !here {
					continue
				}
				if _, exists := out[from]; !exists {
					out[from] = struct{}{}
					changed = true
				}
			}
		}
	}
	result := make([]string, 0, len(out))
	for a := range out {
		result = append(result, a)
	}
	sort.Strings(result)
	return result
}
