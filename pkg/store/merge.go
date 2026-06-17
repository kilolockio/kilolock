package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/davesade/kilolock/internal/tfstate"
)

// 3-way state merge for optimistic POST.
//
// The merge model is:
//
//	base       — the trunk state at the operator's source_serial,
//	             i.e. what they read when they acquired their lock.
//	trunkNow   — the current trunk state at commit time. May equal
//	             base if nobody else committed in the meantime.
//	proposed   — the state the operator is POSTing, computed by
//	             their `terraform apply` from `base`.
//
// The merge produces a state with one resource-instance per address:
//
//   - addresses in `theirsWrites` (changed between base and proposed)
//     are taken from `proposed`,
//   - everything else is taken from `trunkNow`.
//
// If any address appears in BOTH theirsWrites and committedWrites
// (changed between base and trunkNow), the merge fails with a
// WriteSetConflictError. The operator's job is then to refresh
// against trunkNow and re-plan.
//
// Lineage: a non-empty lineage mismatch between proposed and trunkNow
// is always fatal (different states, not different versions). Empty
// lineages are tolerated for legacy states that never had one set.
//
// Output: the merged state's `lineage` and top-level scalars come
// from trunkNow. `serial` is bumped to trunkNow.Serial+1 so the
// state_versions unique constraint catches a concurrent committer
// that beats us into the database. `terraform_version` follows
// trunkNow as well — the operator's CLI version isn't part of the
// merge contract.

// instanceKey is the merge-time identity of a Terraform resource
// instance: the (mode, type, name, module) group plus the instance's
// index_key serialized canonically. Two states share an instance iff
// both have a row with the same key.
//
// The module path is part of the key because two resources at
// `aws_instance.web` in different modules are different things.
type instanceKey struct {
	Mode   string
	Type   string
	Name   string
	Module string
	Index  string // canonical JSON form of IndexKey; "" for IndexNone.
}

// canonicalIndex returns a stable string form of an instance's
// IndexKey. We normalize through tfstate.DecodeIndex rather than
// taking the raw JSON because Terraform sometimes writes `"key"`
// and sometimes `key` for the same identity depending on whether
// the for_each value parsed as a number; the address is what
// matters for the merge.
func canonicalIndex(inst tfstate.ResourceInstance) (string, error) {
	kind, val, err := inst.DecodeIndex()
	if err != nil {
		return "", err
	}
	switch kind {
	case tfstate.IndexNone:
		return "", nil
	case tfstate.IndexInt:
		// Int indices serialize as the bare number.
		return "i:" + val, nil
	case tfstate.IndexString:
		// String indices serialize with a prefix so the int "0" and
		// the string "0" don't collide in the merge map.
		return "s:" + val, nil
	default:
		return "", fmt.Errorf("canonicalIndex: unsupported index kind %v", kind)
	}
}

// instanceRecord is one row in the merge's intermediate
// "instance map" — the instance itself plus enough of its parent
// Resource to rebuild a tfstate.Resource group at the end.
type instanceRecord struct {
	parent   *tfstate.Resource
	instance *tfstate.ResourceInstance
}

// instanceAddressFromKey renders a human-readable Terraform address
// for a key, mirroring tfstate.InstanceAddress. Used for the
// conflict error message and for the address-level write set.
func instanceAddressFromKey(k instanceKey) string {
	var b []byte
	if k.Module != "" {
		b = append(b, k.Module...)
		b = append(b, '.')
	}
	if k.Mode == "data" {
		b = append(b, "data."...)
	}
	b = append(b, k.Type...)
	b = append(b, '.')
	b = append(b, k.Name...)
	switch {
	case k.Index == "":
		// no suffix
	case len(k.Index) > 2 && k.Index[:2] == "i:":
		b = append(b, '[')
		b = append(b, k.Index[2:]...)
		b = append(b, ']')
	case len(k.Index) > 2 && k.Index[:2] == "s:":
		b = append(b, '[')
		b = append(b, '"')
		b = append(b, k.Index[2:]...)
		b = append(b, '"')
		b = append(b, ']')
	}
	return string(b)
}

// buildInstanceMap walks a parsed state and indexes every instance
// by its canonical key. Two instances with the same key in the same
// state is a malformed state; we report it rather than silently
// dropping one.
func buildInstanceMap(s *tfstate.State) (map[instanceKey]instanceRecord, error) {
	out := make(map[instanceKey]instanceRecord, 64)
	for i := range s.Resources {
		r := &s.Resources[i]
		for j := range r.Instances {
			inst := &r.Instances[j]
			idx, err := canonicalIndex(*inst)
			if err != nil {
				return nil, fmt.Errorf("resource %s.%s: %w", r.Type, r.Name, err)
			}
			k := instanceKey{
				Mode:   r.Mode,
				Type:   r.Type,
				Name:   r.Name,
				Module: r.Module,
				Index:  idx,
			}
			if _, dup := out[k]; dup {
				return nil, fmt.Errorf("duplicate instance in state for address %s", instanceAddressFromKey(k))
			}
			out[k] = instanceRecord{parent: r, instance: inst}
		}
	}
	return out, nil
}

// instanceBytes returns a canonical byte-form of a resource instance
// for comparison. We marshal the struct rather than relying on raw
// JSON because the input states may have slightly different
// key-ordering (Terraform's JSON encoder is stable per-version, but
// version skews across operators in real fleets), and the same
// terraform binary will sometimes re-encode an unchanged resource
// with different map-key order on a new run.
//
// The raw-JSON fields (Attributes, SensitiveAttributes, IndexKey)
// are normalized through a decode+re-encode of `any`, which sorts
// map keys alphabetically. This makes the write-set comparison
// SEMANTIC rather than byte-literal: two instances whose
// attribute trees decode to equal values are considered
// unchanged, even if their on-the-wire JSON differs.
//
// Two instances that produce identical bytes after normalization
// are considered unchanged for write-set purposes.
func instanceBytes(inst *tfstate.ResourceInstance) ([]byte, error) {
	norm := *inst
	var err error
	if norm.Attributes, err = canonicalizeRawJSON(norm.Attributes); err != nil {
		return nil, fmt.Errorf("normalize attributes: %w", err)
	}
	if norm.SensitiveAttributes, err = canonicalizeRawJSON(norm.SensitiveAttributes); err != nil {
		return nil, fmt.Errorf("normalize sensitive_attributes: %w", err)
	}
	if norm.IndexKey, err = canonicalizeRawJSON(norm.IndexKey); err != nil {
		return nil, fmt.Errorf("normalize index_key: %w", err)
	}
	b, err := json.Marshal(norm)
	if err != nil {
		return nil, fmt.Errorf("marshal instance: %w", err)
	}
	return b, nil
}

// canonicalizeRawJSON decodes a raw JSON message into a generic
// value and re-encodes it, producing the same bytes for
// semantically-equivalent inputs. Empty or nil input passes through.
func canonicalizeRawJSON(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return raw, nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		// Not valid JSON — keep the original bytes rather than
		// silently changing them. The merge will still see a
		// stable comparison if both sides are equally invalid.
		return raw, nil
	}
	out, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// resourceWriteSet returns the set of instance addresses whose
// instance payload changed between `from` and `to`. An address is
// in the set when:
//
//   - it exists in `to` but not in `from` (added), OR
//   - it exists in `from` but not in `to` (removed), OR
//   - it exists in both and the instance bytes differ (changed).
//
// Comparison is on the marshaled ResourceInstance JSON, which
// includes attributes, sensitive_attributes, dependencies, etc.
func resourceWriteSet(from, to map[instanceKey]instanceRecord) (map[string]struct{}, error) {
	out := make(map[string]struct{})
	for k, fr := range from {
		tr, ok := to[k]
		if !ok {
			out[instanceAddressFromKey(k)] = struct{}{}
			continue
		}
		fb, err := instanceBytes(fr.instance)
		if err != nil {
			return nil, err
		}
		tb, err := instanceBytes(tr.instance)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(fb, tb) {
			out[instanceAddressFromKey(k)] = struct{}{}
		}
	}
	for k := range to {
		if _, ok := from[k]; !ok {
			out[instanceAddressFromKey(k)] = struct{}{}
		}
	}
	return out, nil
}

// outputWriteSet returns the set of output addresses
// (`output.<name>`) whose value, type, or sensitivity changed.
// Outputs are merged by name; two operators writing to the same
// output name is a conflict even if the value happens to be equal.
//
// The address namespace `output.<name>` doesn't collide with
// resource addresses (those never start with `output.`).
func outputWriteSet(from, to map[string]tfstate.Output) map[string]struct{} {
	out := make(map[string]struct{})
	for name, fv := range from {
		tv, ok := to[name]
		if !ok {
			out["output."+name] = struct{}{}
			continue
		}
		if !bytes.Equal(fv.Value, tv.Value) ||
			!bytes.Equal(fv.Type, tv.Type) ||
			fv.Sensitive != tv.Sensitive {
			out["output."+name] = struct{}{}
		}
	}
	for name := range to {
		if _, ok := from[name]; !ok {
			out["output."+name] = struct{}{}
		}
	}
	return out
}

// MergeStates performs a 3-way merge of base, trunkNow, and proposed
// at the instance/output address level and returns either a merged
// *tfstate.State or *WriteSetConflictError describing the conflict.
//
// See the package-level comment block for the merge model.
func MergeStates(base, trunkNow, proposed *tfstate.State) (*tfstate.State, error) {
	if base == nil || trunkNow == nil || proposed == nil {
		return nil, fmt.Errorf("MergeStates: base, trunkNow, and proposed must all be non-nil")
	}
	// Lineage: empty-on-either-side tolerated for backfill cases.
	if proposed.Lineage != "" && trunkNow.Lineage != "" &&
		proposed.Lineage != trunkNow.Lineage {
		return nil, ErrLineageMismatch
	}

	baseMap, err := buildInstanceMap(base)
	if err != nil {
		return nil, fmt.Errorf("index base state: %w", err)
	}
	trunkMap, err := buildInstanceMap(trunkNow)
	if err != nil {
		return nil, fmt.Errorf("index trunk state: %w", err)
	}
	proposedMap, err := buildInstanceMap(proposed)
	if err != nil {
		return nil, fmt.Errorf("index proposed state: %w", err)
	}

	theirsWrites, err := resourceWriteSet(baseMap, proposedMap)
	if err != nil {
		return nil, fmt.Errorf("compute theirs write set: %w", err)
	}
	committedWrites, err := resourceWriteSet(baseMap, trunkMap)
	if err != nil {
		return nil, fmt.Errorf("compute committed write set: %w", err)
	}

	theirsOutputWrites := outputWriteSet(base.Outputs, proposed.Outputs)
	committedOutputWrites := outputWriteSet(base.Outputs, trunkNow.Outputs)

	// Conflict detection: intersection of theirs and committed.
	var conflicts []string
	for addr := range theirsWrites {
		if _, ok := committedWrites[addr]; ok {
			conflicts = append(conflicts, addr)
		}
	}
	for addr := range theirsOutputWrites {
		if _, ok := committedOutputWrites[addr]; ok {
			conflicts = append(conflicts, addr)
		}
	}
	if len(conflicts) > 0 {
		sort.Strings(conflicts)
		return nil, &WriteSetConflictError{
			Addresses:    conflicts,
			LatestSerial: trunkNow.Serial,
		}
	}

	// Build the merged instance map: trunkNow for every address NOT
	// in theirsWrites, proposed for every address in theirsWrites.
	merged := make(map[instanceKey]instanceRecord, len(trunkMap)+len(proposedMap))
	for k, rec := range trunkMap {
		if _, inWrites := theirsWrites[instanceAddressFromKey(k)]; inWrites {
			continue
		}
		merged[k] = rec
	}
	for k, rec := range proposedMap {
		if _, inWrites := theirsWrites[instanceAddressFromKey(k)]; inWrites {
			merged[k] = rec
		}
	}

	// Rebuild the Resources slice from the merged instance map.
	// Resource grouping is by (mode, type, name, module) — the same
	// tuple Terraform writes its groups under. Within a group,
	// instances are sorted by canonical index for determinism.
	resources := regroupResources(merged)

	// Outputs: trunkNow for everything except outputs in theirsOutputWrites.
	mergedOutputs := make(map[string]tfstate.Output, len(trunkNow.Outputs)+len(proposed.Outputs))
	for name, v := range trunkNow.Outputs {
		if _, inWrites := theirsOutputWrites["output."+name]; inWrites {
			continue
		}
		mergedOutputs[name] = v
	}
	for name, v := range proposed.Outputs {
		if _, inWrites := theirsOutputWrites["output."+name]; inWrites {
			mergedOutputs[name] = v
		}
	}

	out := &tfstate.State{
		Version:          4,
		TerraformVersion: trunkNow.TerraformVersion,
		Serial:           trunkNow.Serial + 1,
		Lineage:          trunkNow.Lineage,
		Outputs:          mergedOutputs,
		Resources:        resources,
		CheckResults:     trunkNow.CheckResults,
	}
	return out, nil
}

// regroupResources reassembles a per-instance map back into the
// per-group slice Terraform writes. Groups are sorted by
// (module, mode, type, name) for stable output across runs;
// instances within a group are sorted by canonical index.
//
// The parent metadata (Provider, etc.) is taken from whichever
// record contributed the first instance for the group. In a
// well-formed merge that's consistent across base/trunk/proposed
// since provider references are part of resource identity at
// Terraform's level.
func regroupResources(m map[instanceKey]instanceRecord) []tfstate.Resource {
	type groupKey struct {
		Mode, Type, Name, Module string
	}
	type groupRow struct {
		parent    *tfstate.Resource
		instances []*tfstate.ResourceInstance
		indices   []string
	}
	groups := make(map[groupKey]*groupRow, len(m)/2+1)

	for k, rec := range m {
		gk := groupKey{Mode: k.Mode, Type: k.Type, Name: k.Name, Module: k.Module}
		g, ok := groups[gk]
		if !ok {
			g = &groupRow{parent: rec.parent}
			groups[gk] = g
		}
		g.instances = append(g.instances, rec.instance)
		g.indices = append(g.indices, k.Index)
	}

	keys := make([]groupKey, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Module != keys[j].Module {
			return keys[i].Module < keys[j].Module
		}
		if keys[i].Mode != keys[j].Mode {
			return keys[i].Mode < keys[j].Mode
		}
		if keys[i].Type != keys[j].Type {
			return keys[i].Type < keys[j].Type
		}
		return keys[i].Name < keys[j].Name
	})

	out := make([]tfstate.Resource, 0, len(groups))
	for _, gk := range keys {
		g := groups[gk]
		// Sort instances within the group by canonical index so
		// the result is byte-stable across runs.
		order := make([]int, len(g.instances))
		for i := range order {
			order[i] = i
		}
		sort.Slice(order, func(i, j int) bool {
			return g.indices[order[i]] < g.indices[order[j]]
		})
		sorted := make([]tfstate.ResourceInstance, len(g.instances))
		for newPos, origPos := range order {
			sorted[newPos] = *g.instances[origPos]
		}
		r := tfstate.Resource{
			Mode:      gk.Mode,
			Type:      gk.Type,
			Name:      gk.Name,
			Module:    gk.Module,
			Provider:  g.parent.Provider,
			Instances: sorted,
		}
		out = append(out, r)
	}
	return out
}
