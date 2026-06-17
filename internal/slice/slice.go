// Package slice computes a state-file slice — a subset of a
// Terraform v4 state file containing only the resources whose
// addresses appear in a caller-supplied footprint.
//
// The slice is what v2c materializes in the apply tmp dir's
// terraform.tfstate so that:
//
//   - Terraform's re-plan inside the apply dir produces the same
//     resource_changes set as the operator's original plan (it
//     must see every HCL-declared resource that already exists in
//     trunk, per ADR 0007 spike finding V3);
//   - The trunk itself is never written through Terraform — the
//     orchestrator commits row-level changes back via the v1
//     lifecycle write path, outside of Terraform's view.
//
// Build is intentionally a pure function over already-parsed state
// JSON. The caller does the I/O (fetch trunk from the Store; write
// slice to disk); this package just rearranges bytes.
package slice

import (
	"encoding/json"
	"fmt"
)

// TrunkState mirrors the Terraform v4 state file format at the
// resolution we need to slice it: lineage and serial pass through
// untouched, resources are filtered, outputs are kept as opaque
// JSON (Terraform validates references against them at plan time
// inside the apply dir).
//
// Unknown top-level fields are preserved in Extras so the slice
// round-trips cleanly even when Terraform's state format adds
// fields between versions.
type TrunkState struct {
	Version          int             `json:"version"`
	TerraformVersion string          `json:"terraform_version"`
	Serial           int64           `json:"serial"`
	Lineage          string          `json:"lineage"`
	Outputs          json.RawMessage `json:"outputs,omitempty"`
	CheckResults     json.RawMessage `json:"check_results,omitempty"`
	Resources        []TrunkResource `json:"resources"`
}

// TrunkResource is one row of state.resources[]. Address() rebuilds
// the canonical Terraform address from the (module, mode, type,
// name) tuple; comparison against the footprint set is done on the
// reconstructed address.
type TrunkResource struct {
	Module    string          `json:"module,omitempty"`
	Mode      string          `json:"mode"`
	Type      string          `json:"type"`
	Name      string          `json:"name"`
	Provider  string          `json:"provider"`
	Each      string          `json:"each,omitempty"`
	Instances json.RawMessage `json:"instances"`
}

// Address reconstructs the canonical Terraform address. Mirrors the
// rendering Terraform itself does for resource_changes[].address:
//
//	managed: type.name
//	data:    data.type.name
//	module:  module.<name>.type.name (or module.<name>["k"].type.name
//	         when count/for_each is in effect, but state files
//	         attach the module key to the module field already)
//
// Instance indexing (e.g. `aws_instance.web[0]`) does NOT appear at
// this level — it's per-instance and lives inside Instances. For
// slice purposes we use the resource-group address (the address
// Terraform reservation logic also uses); per-instance refinement
// is unnecessary because slice membership is decided per resource
// group, not per instance.
func (r TrunkResource) Address() string {
	addr := r.Type + "." + r.Name
	if r.Mode == "data" {
		addr = "data." + addr
	}
	if r.Module != "" {
		addr = r.Module + "." + addr
	}
	return addr
}

// Build returns a new TrunkState that contains only the resources
// whose Address() appears in footprint. Lineage, serial, outputs,
// check_results and terraform_version pass through unchanged.
//
// The footprint set is consulted as exact-match equality. Addresses
// in the plan's planned_values block may include count/for_each
// indexing (e.g. `aws_instance.web[0]`); state-file resource groups
// do NOT. The intersection should be computed against the group
// address (the result of TrunkResource.Address()), which is what
// the orchestrator would compare against the plan's resource_changes
// canonical address minus its [N] suffix.
//
// Helper IndexFootprintByGroup below strips indexing so the caller
// passes a usable set into Build.
func Build(trunk *TrunkState, footprint map[string]struct{}) (*TrunkState, error) {
	if trunk == nil {
		return nil, fmt.Errorf("Build: trunk is nil")
	}
	if footprint == nil {
		// Empty footprint = empty slice. Defensive: a nil map is
		// also valid as input but we want the rest of the code to
		// treat it as a real empty set.
		footprint = map[string]struct{}{}
	}
	out := &TrunkState{
		Version:          trunk.Version,
		TerraformVersion: trunk.TerraformVersion,
		Serial:           trunk.Serial,
		Lineage:          trunk.Lineage,
		Outputs:          trunk.Outputs,
		CheckResults:     trunk.CheckResults,
	}
	for _, r := range trunk.Resources {
		if _, keep := footprint[r.Address()]; keep {
			out.Resources = append(out.Resources, r)
		}
	}
	return out, nil
}

// IndexFootprintByGroup converts a slice of plan-style addresses
// (which may contain [N] indexing for count/for_each-expanded
// resources) into a set keyed by the resource-group address — the
// portion before any `[...]` bracket.
//
// Plan addresses:        `aws_instance.web[0]`, `aws_instance.web[1]`
// Slice membership key:  `aws_instance.web`
//
// State files group instances under a single resources[] entry per
// (module, mode, type, name) tuple; the per-instance index sits
// inside `instances[]`. Slicing is therefore done at the group
// level: if any indexed sibling lives in the footprint, the whole
// group is kept.
func IndexFootprintByGroup(addresses []string) map[string]struct{} {
	out := make(map[string]struct{}, len(addresses))
	for _, a := range addresses {
		out[stripIndex(a)] = struct{}{}
	}
	return out
}

// stripIndex removes a trailing `[...]` segment from a Terraform
// address. Robust against multiple segments (e.g.
// `module.web["a"].aws_instance.web[0]`) by walking right-to-left
// and trimming from the FIRST bracket-open we find at the end of
// the string. Bracket pairs in the middle of an address (module
// keys) are left intact.
func stripIndex(addr string) string {
	if len(addr) == 0 {
		return addr
	}
	if addr[len(addr)-1] != ']' {
		return addr
	}
	// scan back to the matching '['
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == '[' {
			return addr[:i]
		}
	}
	return addr
}

// ParseTrunkState decodes a Terraform v4 state JSON document into
// TrunkState. The decoder tolerates unknown top-level fields (Go's
// json.Unmarshal default).
func ParseTrunkState(b []byte) (*TrunkState, error) {
	var t TrunkState
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("decode trunk state: %w", err)
	}
	return &t, nil
}

// MarshalTrunkState is the inverse of ParseTrunkState. Used by the
// apply orchestrator both for the slice file written to the
// terraform tmp directory AND for the merged full-state document
// handed to Store.WriteState.
//
// IMPORTANT: this MUST emit compact JSON, not indented. Each
// TrunkResource.Instances field is a json.RawMessage that carries
// the trunk's per-resource attribute bytes verbatim. encoding/json's
// MarshalIndent re-indents the ENTIRE output (json.Indent rewrites
// whitespace around any token, including inside a RawMessage),
// which changes the per-resource attribute byte stream and thus
// the attributes_hash that v1's normalize() computes when
// WriteState ingests the merged document.
//
// Concretely, with indent enabled:
//   - trunk row attributes (compact Postgres-canonical JSONB)
//     hash to H1
//   - re-marshaled merged row's attributes (re-indented) hash to H2
//   - applyResourceDelta sees H1 != H2 for every non-write-set row
//   - every resource gets close+reinsert → O(state) lifecycle churn
//     instead of O(write_set), which is precisely what v2c-1 was
//     meant to avoid.
//
// Compact marshal preserves the inner RawMessage byte stream as-is,
// so non-write-set rows hash byte-identically and stay on their
// original lifecycle rows. Terraform reads either form happily;
// the slice file in the apply tmp dir is machine-input only.
func MarshalTrunkState(t *TrunkState) ([]byte, error) {
	if t == nil {
		return nil, fmt.Errorf("MarshalTrunkState: trunk is nil")
	}
	return json.Marshal(t)
}
