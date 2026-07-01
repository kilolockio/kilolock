package apply

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/kilolockio/kilolock/internal/plan"
	"github.com/kilolockio/kilolock/internal/slice"
	"github.com/kilolockio/kilolock/internal/tfstate"
	"github.com/kilolockio/kilolock/pkg/store"
)

func buildStateEngineDeltaCommit(postApply *slice.TrunkState, writeSet, deleteSet []string) (store.StateEngineDeltaCommit, error) {
	if postApply == nil {
		return store.StateEngineDeltaCommit{}, fmt.Errorf("post-apply state is nil")
	}
	selected, err := selectSliceResources(postApply, writeSet)
	if err != nil {
		return store.StateEngineDeltaCommit{}, err
	}
	return store.StateEngineDeltaCommit{
		TerraformVersion: postApply.TerraformVersion,
		Lineage:          postApply.Lineage,
		CheckResults:     postApply.CheckResults,
		Resources:        selected,
		WriteSet:         writeSet,
		DeleteSet:        deleteSet,
	}, nil
}

func attachOutputDelta(delta *store.StateEngineDeltaCommit, trunk, postApply *slice.TrunkState) error {
	if delta == nil {
		return fmt.Errorf("delta commit is nil")
	}
	trunkOutputs, err := extractTFStateOutputs(trunk)
	if err != nil {
		return err
	}
	postOutputs, err := extractTFStateOutputs(postApply)
	if err != nil {
		return err
	}
	writes, deletes, err := detectOutputDelta(trunkOutputs, postOutputs)
	if err != nil {
		return err
	}
	delta.OutputWrites = writes
	delta.OutputDeleteSet = deletes
	return nil
}

func validateTrustedStateEngineIntent(spec *plan.PlanSpec, intent *terraformRunIntent) error {
	if spec == nil {
		return fmt.Errorf("trusted state-engine intent: plan spec is nil")
	}
	if intent == nil {
		return fmt.Errorf("trusted state-engine intent: terraform validation replan did not return exact intent")
	}
	wantWrites := slice.IndexFootprintByGroup(spec.WriteSet)
	gotWrites := slice.IndexFootprintByGroup(intent.ExactWriteSet)

	var missing, extra []string
	for addr := range wantWrites {
		if _, ok := gotWrites[addr]; !ok {
			missing = append(missing, addr)
		}
	}
	for addr := range gotWrites {
		if _, ok := wantWrites[addr]; !ok {
			extra = append(extra, addr)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 || len(extra) > 0 {
		return &TrustedStateEngineIntentError{
			MissingWrites: missing,
			ExtraWrites:   extra,
		}
	}

	for _, addr := range intent.DeleteSet {
		addr = strings.TrimSpace(addr)
		if addr == "" {
			continue
		}
		if _, ok := gotWrites[addr]; !ok {
			return &TrustedStateEngineIntentError{DeleteOutsideWrite: addr}
		}
	}
	return nil
}

func detectAppliedWriteSet(trunk, postApply *slice.TrunkState, writeSet []string) ([]string, []string, error) {
	if trunk == nil {
		return nil, nil, fmt.Errorf("trunk state is nil")
	}
	if postApply == nil {
		return nil, nil, fmt.Errorf("post-apply state is nil")
	}
	writeSet = dedupeStringsKeepOrder(writeSet)
	if len(writeSet) == 0 {
		return nil, nil, nil
	}
	trunkByAddr := indexResourcesByAddress(trunk.Resources)
	postByAddr := indexResourcesByAddress(postApply.Resources)
	trunkExact, err := exactStateEntries(trunk)
	if err != nil {
		return nil, nil, err
	}
	postExact, err := exactStateEntries(postApply)
	if err != nil {
		return nil, nil, err
	}

	applied := make([]string, 0, len(writeSet))
	deletes := make([]string, 0)
	for _, addr := range writeSet {
		addr = strings.TrimSpace(addr)
		if addr == "" {
			continue
		}
		if exact, ok := compareExactAddress(addr, trunkByAddr, postByAddr, trunkExact, postExact); ok {
			if exact.changed {
				applied = append(applied, addr)
			}
			if exact.deleted {
				deletes = append(deletes, addr)
			}
		}
	}
	applied = sortedUniqueStrings(applied)
	deletes = sortedUniqueStrings(deletes)
	return applied, deletes, nil
}

type exactCompareResult struct {
	changed bool
	deleted bool
}

type TrustedStateEngineIntentError struct {
	MissingWrites      []string
	ExtraWrites        []string
	DeleteOutsideWrite string
}

func (e *TrustedStateEngineIntentError) Error() string {
	if e == nil {
		return "trusted state-engine intent mismatch"
	}
	if strings.TrimSpace(e.DeleteOutsideWrite) != "" {
		return fmt.Sprintf("trusted state-engine intent mismatch: delete_set contains address outside exact write set: %s", e.DeleteOutsideWrite)
	}
	return fmt.Sprintf("trusted state-engine intent mismatch: write_set differs from plan spec (missing=%v extra=%v)", e.MissingWrites, e.ExtraWrites)
}

type exactStateEntry struct {
	mode     string
	rtype    string
	name     string
	provider string
	module   string
	instance []byte
}

func compareExactAddress(
	addr string,
	trunkByAddr, postByAddr map[string]slice.TrunkResource,
	trunkExact, postExact map[string]exactStateEntry,
) (exactCompareResult, bool) {
	if trunkRow, inTrunk := trunkByAddr[addr]; inTrunk {
		postRow, inPost := postByAddr[addr]
		switch {
		case !inPost:
			return exactCompareResult{changed: true, deleted: true}, true
		default:
			equal, err := trunkResourceEqual(trunkRow, postRow)
			if err != nil {
				return exactCompareResult{}, false
			}
			return exactCompareResult{changed: !equal}, true
		}
	}
	if _, inPost := postByAddr[addr]; inPost {
		return exactCompareResult{changed: true}, true
	}
	trunkValue, inTrunk := trunkExact[addr]
	postValue, inPost := postExact[addr]
	switch {
	case !inTrunk && !inPost:
		return exactCompareResult{}, false
	case inTrunk != inPost:
		return exactCompareResult{changed: true, deleted: inTrunk && !inPost}, true
	default:
		return exactCompareResult{changed: !exactStateEntryEqual(trunkValue, postValue)}, true
	}
}

func exactStateEntries(st *slice.TrunkState) (map[string]exactStateEntry, error) {
	out := make(map[string]exactStateEntry)
	for _, sliceResource := range st.Resources {
		resource, err := sliceResourceToTFStateResource(sliceResource)
		if err != nil {
			return nil, err
		}
		var rawInstances []json.RawMessage
		if len(sliceResource.Instances) > 0 {
			if err := json.Unmarshal(sliceResource.Instances, &rawInstances); err != nil {
				return nil, fmt.Errorf("decode raw instances for %s: %w", sliceResource.Address(), err)
			}
		}
		for i, inst := range resource.Instances {
			addr, err := tfstate.InstanceAddress(resource, inst)
			if err != nil {
				return nil, fmt.Errorf("derive exact address: %w", err)
			}
			if i >= len(rawInstances) {
				return nil, fmt.Errorf("raw instance count mismatch for %s", sliceResource.Address())
			}
			compacted, err := compactRawJSON(rawInstances[i])
			if err != nil {
				return nil, fmt.Errorf("compact exact instance %s: %w", addr, err)
			}
			out[addr] = exactStateEntry{
				mode:     resource.Mode,
				rtype:    resource.Type,
				name:     resource.Name,
				provider: resource.Provider,
				module:   resource.Module,
				instance: compacted,
			}
		}
	}
	return out, nil
}

func extractTFStateOutputs(st *slice.TrunkState) (map[string]tfstate.Output, error) {
	if st == nil {
		return map[string]tfstate.Output{}, nil
	}
	if len(st.Outputs) == 0 {
		return map[string]tfstate.Output{}, nil
	}
	var outputs map[string]tfstate.Output
	if err := json.Unmarshal(st.Outputs, &outputs); err != nil {
		return nil, fmt.Errorf("decode outputs: %w", err)
	}
	if outputs == nil {
		return map[string]tfstate.Output{}, nil
	}
	return outputs, nil
}

func detectOutputDelta(trunkOutputs, postOutputs map[string]tfstate.Output) (map[string]tfstate.Output, []string, error) {
	writes := make(map[string]tfstate.Output)
	deletes := make([]string, 0)

	for name, post := range postOutputs {
		trunk, ok := trunkOutputs[name]
		switch {
		case !ok:
			writes[name] = post
		default:
			trunkBytes, err := json.Marshal(trunk)
			if err != nil {
				return nil, nil, fmt.Errorf("marshal trunk output %s: %w", name, err)
			}
			postBytes, err := json.Marshal(post)
			if err != nil {
				return nil, nil, fmt.Errorf("marshal post output %s: %w", name, err)
			}
			if !bytes.Equal(trunkBytes, postBytes) {
				writes[name] = post
			}
		}
	}
	for name := range trunkOutputs {
		if _, ok := postOutputs[name]; !ok {
			deletes = append(deletes, name)
		}
	}
	deletes = sortedUniqueStrings(deletes)
	if len(writes) == 0 {
		writes = nil
	}
	return writes, deletes, nil
}

func dedupeStringsKeepOrder(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, item := range in {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	return out
}

func sortedUniqueStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := dedupeStringsKeepOrder(in)
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}

func trunkResourceEqual(a, b slice.TrunkResource) (bool, error) {
	if a.Module != b.Module || a.Mode != b.Mode || a.Type != b.Type || a.Name != b.Name || a.Provider != b.Provider || a.Each != b.Each {
		return false, nil
	}
	ab, err := compactRawJSON(a.Instances)
	if err != nil {
		return false, err
	}
	bb, err := compactRawJSON(b.Instances)
	if err != nil {
		return false, err
	}
	return bytes.Equal(ab, bb), nil
}

func exactStateEntryEqual(a, b exactStateEntry) bool {
	return a.mode == b.mode &&
		a.rtype == b.rtype &&
		a.name == b.name &&
		a.provider == b.provider &&
		a.module == b.module &&
		bytes.Equal(a.instance, b.instance)
}

func compactRawJSON(raw []byte) ([]byte, error) {
	if len(raw) == 0 {
		return []byte("null"), nil
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func selectSliceResources(st *slice.TrunkState, writeSet []string) ([]tfstate.Resource, error) {
	if st == nil || len(writeSet) == 0 {
		return nil, nil
	}
	want := make(map[string]struct{}, len(writeSet))
	for _, addr := range writeSet {
		want[addr] = struct{}{}
	}
	out := make([]tfstate.Resource, 0, len(writeSet))
	for _, sliceResource := range st.Resources {
		resource, err := sliceResourceToTFStateResource(sliceResource)
		if err != nil {
			return nil, err
		}
		groupAddr, _ := tfstateResourceGroupAddress(resource)
		if _, keep := want[groupAddr]; keep {
			out = append(out, resource)
			continue
		}
		if len(resource.Instances) == 0 {
			continue
		}
		kept := make([]tfstate.ResourceInstance, 0, len(resource.Instances))
		for _, inst := range resource.Instances {
			addr, err := tfstate.InstanceAddress(resource, inst)
			if err != nil {
				return nil, fmt.Errorf("derive resource address: %w", err)
			}
			if _, keep := want[addr]; keep {
				kept = append(kept, inst)
			}
		}
		if len(kept) == 0 {
			continue
		}
		resource.Instances = kept
		out = append(out, resource)
	}
	return out, nil
}

func sliceResourceToTFStateResource(in slice.TrunkResource) (tfstate.Resource, error) {
	resource := tfstate.Resource{
		Mode:     in.Mode,
		Type:     in.Type,
		Name:     in.Name,
		Provider: in.Provider,
		Module:   in.Module,
	}
	if len(in.Instances) == 0 {
		return resource, nil
	}
	if err := json.Unmarshal(in.Instances, &resource.Instances); err != nil {
		return tfstate.Resource{}, fmt.Errorf("decode resource instances for %s: %w", in.Address(), err)
	}
	return resource, nil
}

func tfstateResourceGroupAddress(r tfstate.Resource) (string, bool) {
	addr := r.Type + "." + r.Name
	if strings.TrimSpace(r.Mode) == "data" {
		addr = "data." + addr
	}
	if strings.TrimSpace(r.Module) != "" {
		addr = strings.TrimSpace(r.Module) + "." + addr
	}
	return addr, true
}
