package apply

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/kilolockio/kilolock/internal/slice"
)

// mergeResult bundles the byte-level output of buildMergedState
// with the auditing counters the orchestrator records on the
// apply_run row.
type mergeResult struct {
	// MergedBytes is the JSON state document the orchestrator
	// hands to Store.WriteState. Its serial is trunkSerial + 1.
	MergedBytes []byte

	// NewSerial == trunkSerial + 1.
	NewSerial int64

	// AppliedAddresses is the subset of writeSet whose attributes
	// actually differ between trunk and post-apply (or that were
	// created/deleted by the apply). Counted into apply_runs.
	// resources_applied for the audit row.
	AppliedAddresses []string
}

// buildMergedState computes the new full-state document that
// becomes the next state_version for the trunk after an apply.
//
// Inputs:
//
//   - trunk: the current trunk state we read at commit time. Its
//     serial is the basis for the new serial.
//   - postApply: the state file Terraform left in the apply tmp
//     directory. Carries the new attributes for write_set members,
//     plus byte-canonicalized (key-sorted) copies of everything
//     else from the slice.
//   - writeSet: the addresses we acquired write reservations on.
//     This is the authoritative list of "what the operator asked
//     to change" — the merge replaces only these rows.
//   - hclFootprint: the set of addresses the operator's HCL
//     declares (output of plan.ExtractHCLFootprint). Used for the
//     safety check below.
//
// Algorithm:
//
//  1. Validate every resource in postApply has an Address() that
//     appears in hclFootprint. A surprise resource (terraform
//     created something the operator's HCL doesn't describe) is
//     a fail-loud bug — we have no reservation to protect that
//     write.
//
//  2. Build a working copy of trunk.Resources keyed by Address().
//
//  3. For each address in writeSet:
//
//     a. If the address appears in postApply, replace the trunk
//     copy with the post-apply row. Its attributes carry the
//     new values from terraform's apply.
//
//     b. If the address is absent from postApply, the resource
//     was deleted; remove it from the working copy.
//
//  4. Build a result state with trunk's lineage / version /
//     terraform_version, the resources working copy serialized as
//     a sorted list, and outputs taken from postApply (terraform
//     updates outputs during apply).
//
//  5. Stamp serial = trunk.Serial + 1.
//
// Non-write-set rows are NEVER taken from postApply. This is the
// row-level-commit invariant: the merged state's attributes for
// non-write-set resources are exactly what the trunk had at commit
// time — byte-identical, no re-canonicalization, no opportunity to
// clobber a parallel apply's writes.
func buildMergedState(
	trunk *slice.TrunkState,
	postApply *slice.TrunkState,
	writeSet []string,
	hclFootprint map[string]struct{},
) (*mergeResult, error) {
	if trunk == nil {
		return nil, fmt.Errorf("buildMergedState: trunk is nil")
	}
	if postApply == nil {
		return nil, fmt.Errorf("buildMergedState: postApply is nil")
	}

	// Step 1: surprise-resource check. Terraform must not create
	// anything outside the operator's HCL footprint; if it does,
	// we have no reservation defending that address and a parallel
	// writer could be modifying it right now.
	for _, r := range postApply.Resources {
		addr := r.Address()
		if _, ok := hclFootprint[addr]; !ok {
			return nil, fmt.Errorf("post-apply state contains resource %q not in HCL footprint: refusing to commit (no reservation held)", addr)
		}
	}

	postByAddr := indexResourcesByAddress(postApply.Resources)
	trunkByAddr := indexResourcesByAddress(trunk.Resources)

	writeSetIdx := make(map[string]struct{}, len(writeSet))
	for _, a := range writeSet {
		writeSetIdx[a] = struct{}{}
	}

	// Address-keyed working copy. We rebuild the resource slice
	// from this map at the end so iteration order is sorted (which
	// matters: state files are read by humans and diffs are easier
	// when row order is stable across writes).
	workingCopy := make(map[string]slice.TrunkResource, len(trunkByAddr))
	for addr, r := range trunkByAddr {
		workingCopy[addr] = r
	}

	// Plan-style write_set may include addresses with indexing
	// suffixes (count/for_each-expanded). State resources are
	// keyed at the group level (no [N] suffix). Collapse before
	// matching so per-instance addresses don't slip past as
	// "not in write_set". The slice package's same-named helper
	// is intentionally re-used so this matches the slicing rule.
	writeSetGroups := slice.IndexFootprintByGroup(writeSet)

	applied := make([]string, 0, len(writeSetGroups))

	// Step 3: process every write_set address.
	for addr := range writeSetGroups {
		_, inTrunk := trunkByAddr[addr]
		postRow, inPost := postByAddr[addr]
		switch {
		case inPost:
			// Either an update (trunk had it; post-apply has it
			// with new content) or a create (trunk didn't have it).
			workingCopy[addr] = postRow
			applied = append(applied, addr)
		case inTrunk:
			// Trunk had it, post-apply doesn't — terraform
			// deleted it. Drop from working copy.
			delete(workingCopy, addr)
			applied = append(applied, addr)
		default:
			// Neither side has it. The plan promised a write
			// here but nothing came back; this is unusual but
			// not necessarily fatal (could be a plan that
			// somehow had a stale-noop entry for an already-
			// gone address). Record it as applied=0 — the
			// audit row should reflect "we tried and there
			// was nothing to do".
		}
	}

	// Step 4: rebuild resources slice from the address-keyed
	// working copy, sorted by Address() so output is stable.
	addrs := make([]string, 0, len(workingCopy))
	for a := range workingCopy {
		addrs = append(addrs, a)
	}
	sort.Strings(addrs)
	merged := &slice.TrunkState{
		Version:          trunk.Version,
		TerraformVersion: trunk.TerraformVersion,
		Lineage:          trunk.Lineage,
		Serial:           trunk.Serial + 1,
		// Outputs come from the post-apply state — terraform
		// may have updated output values, and we want those
		// reflected in the new version. The trunk's outputs are
		// abandoned (they were stale by definition).
		Outputs:      postApply.Outputs,
		CheckResults: postApply.CheckResults,
		Resources:    make([]slice.TrunkResource, 0, len(addrs)),
	}
	if merged.Lineage == "" {
		merged.Lineage = postApply.Lineage
	}
	if merged.TerraformVersion == "" || merged.TerraformVersion == "0.0.0" {
		merged.TerraformVersion = postApply.TerraformVersion
	}
	for _, a := range addrs {
		merged.Resources = append(merged.Resources, workingCopy[a])
	}

	mergedBytes, err := marshalCanonical(merged)
	if err != nil {
		return nil, fmt.Errorf("marshal merged state: %w", err)
	}

	sort.Strings(applied)
	return &mergeResult{
		MergedBytes:      mergedBytes,
		NewSerial:        merged.Serial,
		AppliedAddresses: applied,
	}, nil
}

// indexResourcesByAddress builds an address-keyed map of the
// resource group rows. Duplicates (which would indicate a
// malformed state) are detected and the second occurrence wins —
// we don't fail because real terraform state files never
// duplicate.
func indexResourcesByAddress(rs []slice.TrunkResource) map[string]slice.TrunkResource {
	out := make(map[string]slice.TrunkResource, len(rs))
	for _, r := range rs {
		out[r.Address()] = r
	}
	return out
}

// marshalCanonical serializes a TrunkState with stable field
// order. We use the slice package's MarshalTrunkState which uses
// json.MarshalIndent — Go's json package emits fields in struct
// declaration order, so the output is already deterministic for a
// given input. The wrapper exists so any future canonicalization
// (e.g. sorting JSON object keys inside Outputs) has one
// chokepoint.
func marshalCanonical(t *slice.TrunkState) ([]byte, error) {
	return slice.MarshalTrunkState(t)
}

// validatePostApplyHasNoSurprises is exposed for the orchestrator
// (and for unit tests) to run the safety check independently of
// the merge. Returns the violating addresses sorted, or nil when
// everything is in the footprint.
func validatePostApplyHasNoSurprises(
	postApply *slice.TrunkState,
	hclFootprint map[string]struct{},
) []string {
	if postApply == nil {
		return nil
	}
	var bad []string
	for _, r := range postApply.Resources {
		a := r.Address()
		if _, ok := hclFootprint[a]; !ok {
			bad = append(bad, a)
		}
	}
	sort.Strings(bad)
	return bad
}

// jsonRawEqual reports whether two json.RawMessage values
// represent the same JSON document, up to key reordering. The
// post-apply state's attribute objects come back with alphabetized
// keys; the trunk's may not. Byte equality is not a valid
// "unchanged?" test — semantic equality is.
//
// Implementation: decode both sides into any (which sorts map
// keys when re-marshaled), then byte-compare the re-marshaled
// forms. Allocation-heavy but only used for inner-loop comparison
// in v2c-2's re-plan validation; orchestrator.go does NOT call
// this directly (writeSet is the source of truth for what to
// merge).
func jsonRawEqual(a, b json.RawMessage) (bool, error) {
	if len(a) == 0 && len(b) == 0 {
		return true, nil
	}
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		return false, err
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		return false, err
	}
	ab, err := json.Marshal(av)
	if err != nil {
		return false, err
	}
	bb, err := json.Marshal(bv)
	if err != nil {
		return false, err
	}
	if len(ab) != len(bb) {
		return false, nil
	}
	for i := range ab {
		if ab[i] != bb[i] {
			return false, nil
		}
	}
	return true, nil
}
