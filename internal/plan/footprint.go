package plan

import "sort"

// ExtractHCLFootprint returns the canonical addresses of every
// resource the operator's HCL configuration declares, including
// nested modules and count/for_each expansions. Source of truth is
// planned_values.root_module — by the time Terraform produces this
// block, all dynamic indexing (count/for_each) has been resolved to
// concrete addresses.
//
// This is the set the apply orchestrator's state slice must contain
// (per ADR 0007 spike finding V3): if any HCL-described resource is
// missing from the slice, Terraform's re-plan inside the apply tmp
// dir will treat it as `create`, diverging from the operator's
// reviewed plan.
//
// The result is sorted lexicographically for stable output.
//
// Data sources are included by default (mode='data' rows appear in
// planned_values too). They live in the state file like managed
// resources, so the slice has to carry them as well — otherwise
// terraform re-plans them as `read` and we get a plan delta against
// the original review.
func ExtractHCLFootprint(f *File) []string {
	if f == nil {
		return nil
	}
	seen := map[string]struct{}{}
	walkPlannedModule(f.PlannedValues.RootModule, seen)
	out := make([]string, 0, len(seen))
	for a := range seen {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// walkPlannedModule deposits every resource address it finds in
// seen, then recurses into child_modules. The PlannedResource's
// Address is already module-qualified, so no prefix stitching is
// needed here (unlike walkConfigModule, which works on bare
// addresses inside expressions).
func walkPlannedModule(m PlannedModule, seen map[string]struct{}) {
	for _, r := range m.Resources {
		if r.Address != "" {
			seen[r.Address] = struct{}{}
		}
	}
	for _, c := range m.ChildModules {
		walkPlannedModule(c, seen)
	}
}
