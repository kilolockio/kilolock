package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kilolockio/kilolock/internal/configscope"
	"github.com/kilolockio/kilolock/internal/plan"
	"github.com/kilolockio/kilolock/internal/tfstate"
	"github.com/kilolockio/kilolock/pkg/config"
	"github.com/kilolockio/kilolock/pkg/store"
)

const (
	cliProtocolTerraformHTTP = "terraform-http"
	cliProtocolStateEngine   = "state-engine"
)

type stateEngineClient struct {
	api *apiClient
}

type stateEngineResolveResponse struct {
	State   string `json:"state"`
	StateID string `json:"state_id"`
	Lineage string `json:"lineage"`
	Serial  int64  `json:"serial"`
}

type stateEngineScopeExpandResponse struct {
	ScopeContract struct {
		FetchAddresses       []string `json:"fetch_addresses"`
		WriteAddresses       []string `json:"write_addresses"`
		ReadAddresses        []string `json:"read_addresses"`
		ConfigRequiredNodes  []string `json:"config_required_nodes"`
		RemovedConfigNodes   []string `json:"removed_config_nodes"`
		MissingFromState     []string `json:"missing_from_state"`
		UndeployedCandidates []string `json:"undeployed_candidates"`
		UnknownMissing       []string `json:"unknown_missing_from_state"`
		Confidence           string   `json:"confidence"`
		Notes                []string `json:"notes"`
		Diagnostics          struct {
			GraphCacheHit         bool  `json:"graph_cache_hit"`
			RealizedResourceCount int   `json:"realized_resource_count"`
			DependencyEdgeCount   int   `json:"dependency_edge_count"`
			InventoryScanCount    int   `json:"inventory_scan_count"`
			WalkedNodeCount       int   `json:"walked_node_count"`
			ConfigNodeCount       int   `json:"config_node_count"`
			ModuleSelectorCount   int   `json:"module_selector_count"`
			FetchAddressCount     int   `json:"fetch_address_count"`
			WriteAddressCount     int   `json:"write_address_count"`
			ReadAddressCount      int   `json:"read_address_count"`
			ExpandDurationMs      int64 `json:"expand_duration_ms"`
		} `json:"diagnostics"`
	} `json:"scope_contract"`
	ContractSource       string   `json:"contract_source"`
	FetchAddresses       []string `json:"fetch_addresses"`
	RealizedWriteClosure []string `json:"realized_write_closure"`
	RealizedReadClosure  []string `json:"realized_read_closure"`
	MissingFromState     []string `json:"missing_from_state"`
	UndeployedCandidates []string `json:"undeployed_candidates"`
	UnknownMissing       []string `json:"unknown_missing_from_state"`
	Confidence           string   `json:"confidence"`
	Diagnostics          struct {
		GraphCacheHit         bool `json:"graph_cache_hit"`
		RealizedResourceCount int  `json:"realized_resource_count"`
		DependencyEdgeCount   int  `json:"dependency_edge_count"`
		InventoryScanCount    int  `json:"inventory_scan_count"`
	} `json:"diagnostics"`
}

type stateEngineSliceEnvelope struct {
	State   string `json:"state"`
	StateID string `json:"state_id"`
	Lineage string `json:"lineage"`
	Serial  int64  `json:"serial"`
	Slice   struct {
		Resources []store.StateEngineResource `json:"resources"`
		Metadata  struct {
			Diagnostics struct {
				RequestedAddressCount int   `json:"requested_address_count"`
				MaterializedCount     int   `json:"materialized_count"`
				MaterializeDurationMs int64 `json:"materialize_duration_ms"`
			} `json:"diagnostics"`
		} `json:"metadata"`
	} `json:"slice"`
}

type stateEngineMutationResponse struct {
	OK      bool                           `json:"ok"`
	Preview *store.ResourceMutationPreview `json:"preview"`
	Version *store.StateVersionInfo        `json:"version"`
}

type stateEngineRollbackResponse struct {
	OK      bool                           `json:"ok"`
	Preview *store.ResourceRollbackPreview `json:"preview"`
	Version *store.StateVersionInfo        `json:"version"`
}

type stateEngineScopedSliceResult struct {
	Raw                    []byte
	Serial                 *int64
	DiscoveryEngine        string
	ResolveDurationMs      int64
	ExpandDurationMs       int64
	SliceFetchDurationMs   int64
	SliceResourceCount     int
	GraphCacheHit          bool
	RealizedResourceCount  int
	DependencyEdgeCount    int
	InventoryScanCount     int
	WalkedNodeCount        int
	ConfigNodeCount        int
	ModuleSelectorCount    int
	FetchAddressCount      int
	WriteAddressCount      int
	ReadAddressCount       int
	ServerExpandMs         int64
	SliceRequestedCount    int
	SliceMaterializedCount int
	ServerSliceMs          int64
	FetchAddresses         []string
	WriteAddresses         []string
	ConfigRequiredNodes    []string
	RemovedConfigNodes     []string
	MissingFromState       []string
	UndeployedCandidates   []string
	UnknownMissing         []string
	Confidence             string
	Notes                  []string
}

var errStateEngineUnsafeScope = errors.New("state-engine unsafe scope closure")

type stateEngineScopeSafetyError struct {
	UnknownMissing []string
	Confidence     string
}

func (e *stateEngineScopeSafetyError) Error() string {
	if e == nil {
		return errStateEngineUnsafeScope.Error()
	}
	if len(e.UnknownMissing) == 0 {
		return fmt.Sprintf("%s (confidence=%s)", errStateEngineUnsafeScope, strings.TrimSpace(e.Confidence))
	}
	return fmt.Sprintf("%s: backend could not classify %s from realized state", errStateEngineUnsafeScope, strings.Join(e.UnknownMissing, ", "))
}

func (e *stateEngineScopeSafetyError) Unwrap() error { return errStateEngineUnsafeScope }

func resolvedCLIProtocol(cfg config.Config) string {
	switch strings.ToLower(strings.TrimSpace(cfg.Protocol)) {
	case "", "terraform-http", "http", "backend":
		return cliProtocolTerraformHTTP
	case "state-engine":
		return cliProtocolStateEngine
	default:
		return strings.ToLower(strings.TrimSpace(cfg.Protocol))
	}
}

func newStateEngineClientFromBackend(cwd string) (*stateEngineClient, error) {
	api, err := newAPIClientFromBackend(cwd)
	if err != nil {
		return nil, err
	}
	return &stateEngineClient{api: api}, nil
}

func (c *stateEngineClient) resolve(ctx context.Context, stateName string) (*stateEngineResolveResponse, error) {
	var out stateEngineResolveResponse
	if err := c.api.postJSON(ctx, "/state-engine/state/resolve", stateName, map[string]any{
		"state": stateName,
	}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *stateEngineClient) expand(ctx context.Context, stateName string, selectors []map[string]string, writeCandidates, readCandidates, undeployed []string) (*stateEngineScopeExpandResponse, error) {
	var out stateEngineScopeExpandResponse
	if err := c.api.postJSON(ctx, "/state-engine/scope/expand", stateName, map[string]any{
		"state":     stateName,
		"selectors": selectors,
		"client_context": map[string]any{
			"explicit_write_candidates":    dedupeSortedStrings(writeCandidates),
			"explicit_read_candidates":     dedupeSortedStrings(readCandidates),
			"undeployed_config_candidates": dedupeSortedStrings(undeployed),
		},
	}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *stateEngineClient) expandWithIntent(ctx context.Context, stateName string, selectors []map[string]string, intent *configscope.Intent) (*stateEngineScopeExpandResponse, error) {
	configNodes := make([]map[string]any, 0, len(intent.ConfigNodes))
	for _, node := range intent.ConfigNodes {
		configNodes = append(configNodes, map[string]any{
			"address":      strings.TrimSpace(node.Address),
			"dependencies": dedupeSortedStrings(node.Dependencies),
		})
	}
	var out stateEngineScopeExpandResponse
	if err := c.api.postJSON(ctx, "/state-engine/scope/expand", stateName, map[string]any{
		"state":     stateName,
		"selectors": selectors,
		"client_context": map[string]any{
			"explicit_write_candidates":    dedupeSortedStrings(intent.ExplicitWriteCandidates),
			"explicit_read_candidates":     dedupeSortedStrings(intent.ExplicitReadCandidates),
			"undeployed_config_candidates": dedupeSortedStrings(intent.UndeployedConfigCandidates),
			"removed_config_candidates":    dedupeSortedStrings(intent.RemovedConfigCandidates),
			"config_nodes":                 configNodes,
		},
	}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *stateEngineClient) fetchSlice(ctx context.Context, stateName string, baseSerial int64, addresses []string) (*stateEngineSliceEnvelope, error) {
	var out stateEngineSliceEnvelope
	if err := c.api.postJSON(ctx, "/state-engine/state/slice", stateName, map[string]any{
		"state":       stateName,
		"base_serial": baseSerial,
		"addresses":   dedupeSortedStrings(addresses),
	}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func newStateEngineClient(api *apiClient) *stateEngineClient {
	return &stateEngineClient{api: api}
}

func (c *stateEngineClient) previewRemove(ctx context.Context, stateName, address string) (*store.ResourceMutationPreview, error) {
	var out stateEngineMutationResponse
	if err := c.api.postJSON(ctx, "/state-engine/resource-remove/preview", stateName, map[string]any{
		"state":   stateName,
		"address": address,
	}, &out); err != nil {
		return nil, err
	}
	return out.Preview, nil
}

func (c *stateEngineClient) applyRemove(ctx context.Context, stateName, address, actor string) (*stateEngineMutationResponse, error) {
	var out stateEngineMutationResponse
	if err := c.api.postJSON(ctx, "/state-engine/resource-remove/apply", stateName, map[string]any{
		"state":   stateName,
		"address": address,
		"actor":   actor,
	}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *stateEngineClient) previewMove(ctx context.Context, stateName, from, to string) (*store.ResourceMutationPreview, error) {
	var out stateEngineMutationResponse
	if err := c.api.postJSON(ctx, "/state-engine/resource-move/preview", stateName, map[string]any{
		"state":   stateName,
		"address": from,
		"to":      to,
	}, &out); err != nil {
		return nil, err
	}
	return out.Preview, nil
}

func (c *stateEngineClient) applyMove(ctx context.Context, stateName, from, to, actor string) (*stateEngineMutationResponse, error) {
	var out stateEngineMutationResponse
	if err := c.api.postJSON(ctx, "/state-engine/resource-move/apply", stateName, map[string]any{
		"state":   stateName,
		"address": from,
		"to":      to,
		"actor":   actor,
	}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *stateEngineClient) previewRollback(ctx context.Context, stateName, address, ref string) (*store.ResourceRollbackPreview, error) {
	var out stateEngineRollbackResponse
	if err := c.api.postJSON(ctx, "/state-engine/resource-rollback/preview", stateName, map[string]any{
		"state":   stateName,
		"address": address,
		"to":      ref,
	}, &out); err != nil {
		return nil, err
	}
	return out.Preview, nil
}

func (c *stateEngineClient) applyRollback(ctx context.Context, stateName, address, ref, actor string) (*stateEngineRollbackResponse, error) {
	var out stateEngineRollbackResponse
	if err := c.api.postJSON(ctx, "/state-engine/resource-rollback/apply", stateName, map[string]any{
		"state":   stateName,
		"address": address,
		"to":      ref,
		"actor":   actor,
	}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func fetchScopedStateViaStateEngineForFiles(ctx context.Context, cwd, configDir string, scope *plan.FileScope) (*stateEngineScopedSliceResult, error) {
	intent, err := configscope.DiscoverForFiles(configDir, scope)
	if err != nil {
		return nil, err
	}
	if !intentHasNativeScope(intent) {
		return nil, fmt.Errorf("state-engine scoped plan: selected files declare no root resources/modules")
	}
	return fetchScopedStateViaStateEngine(ctx, cwd, intent)
}

func fetchScopedStateViaStateEngineForTargets(ctx context.Context, cwd, configDir string, targets []string) (*stateEngineScopedSliceResult, error) {
	intent, err := configscope.DiscoverForTargets(configDir, targets)
	if err != nil {
		return nil, err
	}
	return fetchScopedStateViaStateEngine(ctx, cwd, intent)
}

func fetchScopedStateViaStateEngine(ctx context.Context, cwd string, intent *configscope.Intent) (*stateEngineScopedSliceResult, error) {
	client, err := newStateEngineClientFromBackend(cwd)
	if err != nil {
		return nil, err
	}
	stateName := strings.TrimSpace(client.api.defaultStateName)
	if stateName == "" {
		return nil, fmt.Errorf("state-engine scoped plan: no state name discovered from backend")
	}
	resolveStarted := time.Now()
	resolved, err := client.resolve(ctx, stateName)
	resolveDuration := time.Since(resolveStarted)
	if err != nil {
		return nil, err
	}

	selectors := make([]map[string]string, 0, len(intent.Selectors))
	for _, selector := range intent.Selectors {
		selectors = append(selectors, map[string]string{"kind": selector.Kind, "value": selector.Value})
	}
	expandStarted := time.Now()
	expanded, err := client.expandWithIntent(ctx, stateName, selectors, intent)
	expandDuration := time.Since(expandStarted)
	if err != nil {
		return nil, err
	}
	unknownMissing := expanded.UnknownMissing
	confidence := expanded.Confidence
	addresses := append([]string{}, expanded.RealizedWriteClosure...)
	addresses = append(addresses, expanded.RealizedReadClosure...)
	configRequiredNodes := []string(nil)
	if len(expanded.ScopeContract.FetchAddresses) > 0 || expanded.ContractSource != "" {
		if len(expanded.ScopeContract.UnknownMissing) > 0 {
			unknownMissing = expanded.ScopeContract.UnknownMissing
		}
		if strings.TrimSpace(expanded.ScopeContract.Confidence) != "" {
			confidence = expanded.ScopeContract.Confidence
		}
		configRequiredNodes = append([]string{}, expanded.ScopeContract.ConfigRequiredNodes...)
		addresses = append([]string{}, expanded.ScopeContract.FetchAddresses...)
		if len(addresses) == 0 {
			addresses = append([]string{}, expanded.FetchAddresses...)
		}
	}
	if len(unknownMissing) > 0 || strings.EqualFold(strings.TrimSpace(confidence), "unsafe") {
		return nil, &stateEngineScopeSafetyError{
			UnknownMissing: dedupeSortedStrings(unknownMissing),
			Confidence:     strings.TrimSpace(confidence),
		}
	}
	sliceStarted := time.Now()
	sliceDoc, err := client.fetchSlice(ctx, stateName, resolved.Serial, addresses)
	sliceDuration := time.Since(sliceStarted)
	if err != nil {
		return nil, err
	}
	raw, err := marshalStateEngineSliceToTerraformState(sliceDoc.Lineage, sliceDoc.Serial, sliceDoc.Slice.Resources)
	if err != nil {
		return nil, err
	}
	serial := sliceDoc.Serial
	return &stateEngineScopedSliceResult{
		Raw:                    raw,
		Serial:                 &serial,
		DiscoveryEngine:        strings.TrimSpace(intent.DiscoveryEngine),
		ResolveDurationMs:      resolveDuration.Milliseconds(),
		ExpandDurationMs:       expandDuration.Milliseconds(),
		SliceFetchDurationMs:   sliceDuration.Milliseconds(),
		SliceResourceCount:     len(sliceDoc.Slice.Resources),
		GraphCacheHit:          expanded.ScopeContract.Diagnostics.GraphCacheHit,
		RealizedResourceCount:  expanded.ScopeContract.Diagnostics.RealizedResourceCount,
		DependencyEdgeCount:    expanded.ScopeContract.Diagnostics.DependencyEdgeCount,
		InventoryScanCount:     expanded.ScopeContract.Diagnostics.InventoryScanCount,
		WalkedNodeCount:        expanded.ScopeContract.Diagnostics.WalkedNodeCount,
		ConfigNodeCount:        expanded.ScopeContract.Diagnostics.ConfigNodeCount,
		ModuleSelectorCount:    expanded.ScopeContract.Diagnostics.ModuleSelectorCount,
		FetchAddressCount:      expanded.ScopeContract.Diagnostics.FetchAddressCount,
		WriteAddressCount:      expanded.ScopeContract.Diagnostics.WriteAddressCount,
		ReadAddressCount:       expanded.ScopeContract.Diagnostics.ReadAddressCount,
		ServerExpandMs:         expanded.ScopeContract.Diagnostics.ExpandDurationMs,
		SliceRequestedCount:    sliceDoc.Slice.Metadata.Diagnostics.RequestedAddressCount,
		SliceMaterializedCount: sliceDoc.Slice.Metadata.Diagnostics.MaterializedCount,
		ServerSliceMs:          sliceDoc.Slice.Metadata.Diagnostics.MaterializeDurationMs,
		FetchAddresses:         dedupeSortedStrings(addresses),
		WriteAddresses:         dedupeSortedStrings(expanded.ScopeContract.WriteAddresses),
		ConfigRequiredNodes:    dedupeSortedStrings(configRequiredNodes),
		RemovedConfigNodes:     dedupeSortedStrings(expanded.ScopeContract.RemovedConfigNodes),
		MissingFromState:       dedupeSortedStrings(expanded.ScopeContract.MissingFromState),
		UndeployedCandidates:   dedupeSortedStrings(expanded.ScopeContract.UndeployedCandidates),
		UnknownMissing:         dedupeSortedStrings(unknownMissing),
		Confidence:             strings.TrimSpace(confidence),
		Notes:                  append(append([]string{}, intent.DiscoveryNotes...), expanded.ScopeContract.Notes...),
	}, nil
}

func stateEnginePlanModeForScopedResult(scoped *stateEngineScopedSliceResult) string {
	if scoped == nil {
		return ""
	}
	if strings.TrimSpace(scoped.DiscoveryEngine) == configscope.EngineHeuristic {
		for _, note := range scoped.Notes {
			if strings.Contains(strings.ToLower(strings.TrimSpace(note)), "fell back from "+configscope.EngineOpenTofu+" to "+configscope.EngineHeuristic) {
				return "native-slice-with-discovery-fallback"
			}
		}
	}
	return "native-slice"
}

func stateEngineFallbackPlanMeta(reason string) *plan.StateEnginePlanMetadata {
	reason = strings.TrimSpace(reason)
	meta := &plan.StateEnginePlanMetadata{
		Mode:           "full-trunk-fallback",
		FallbackReason: reason,
	}
	if reason != "" {
		meta.Notes = []string{reason}
	}
	return meta
}

func shouldFallbackStateEngineScoped(err error) (bool, string) {
	if err == nil {
		return false, ""
	}
	var unsafeErr *stateEngineScopeSafetyError
	if errors.As(err, &unsafeErr) || errors.Is(err, errStateEngineUnsafeScope) {
		return false, "backend could not prove a safe native slice for this scope"
	}
	return true, "native scoped state-engine path unavailable; falling back to full trunk"
}

func intentHasNativeScope(intent *configscope.Intent) bool {
	if intent == nil {
		return false
	}
	return len(intent.PlanningTargets) > 0 ||
		len(intent.Selectors) > 0 ||
		len(intent.ExplicitWriteCandidates) > 0 ||
		len(intent.ExplicitReadCandidates) > 0 ||
		len(intent.RemovedConfigCandidates) > 0
}

func marshalStateEngineSliceToTerraformState(lineage string, serial int64, resources []store.StateEngineResource) ([]byte, error) {
	type groupKey struct {
		Module   string
		Mode     string
		Type     string
		Name     string
		Provider string
	}
	groups := make(map[groupKey][]tfstate.ResourceInstance)
	order := make([]groupKey, 0)
	seen := make(map[groupKey]struct{})
	for _, resource := range resources {
		key := groupKey{
			Module:   strings.TrimSpace(resource.ModulePath),
			Mode:     strings.TrimSpace(resource.Mode),
			Type:     strings.TrimSpace(resource.Type),
			Name:     strings.TrimSpace(resource.Name),
			Provider: strings.TrimSpace(resource.Provider),
		}
		if _, ok := seen[key]; !ok {
			seen[key] = struct{}{}
			order = append(order, key)
		}
		indexKey, err := encodeStateEngineIndexKey(resource.IndexKind, resource.IndexValue)
		if err != nil {
			return nil, fmt.Errorf("marshal state-engine slice %s: %w", resource.Address, err)
		}
		groups[key] = append(groups[key], tfstate.ResourceInstance{
			SchemaVersion:       0,
			Attributes:          resource.Attributes,
			SensitiveAttributes: resource.SensitivePaths,
			Dependencies:        append([]string(nil), resource.Dependencies...),
			IndexKey:            indexKey,
		})
	}
	sort.Slice(order, func(i, j int) bool {
		a := order[i]
		b := order[j]
		if a.Module != b.Module {
			return a.Module < b.Module
		}
		if a.Mode != b.Mode {
			return a.Mode < b.Mode
		}
		if a.Type != b.Type {
			return a.Type < b.Type
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.Provider < b.Provider
	})
	state := tfstate.State{
		Version:          4,
		TerraformVersion: "0.0.0",
		Serial:           serial,
		Lineage:          lineage,
		Outputs:          map[string]tfstate.Output{},
		Resources:        make([]tfstate.Resource, 0, len(order)),
	}
	for _, key := range order {
		instances := groups[key]
		sort.Slice(instances, func(i, j int) bool {
			return compareStateEngineInstances(instances[i], instances[j]) < 0
		})
		state.Resources = append(state.Resources, tfstate.Resource{
			Mode:      key.Mode,
			Type:      key.Type,
			Name:      key.Name,
			Provider:  key.Provider,
			Module:    key.Module,
			Instances: instances,
		})
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return nil, fmt.Errorf("marshal terraform state slice: %w", err)
	}
	return raw, nil
}

func encodeStateEngineIndexKey(kind, value string) (json.RawMessage, error) {
	switch strings.TrimSpace(kind) {
	case "", "none":
		return nil, nil
	case "int":
		value = strings.TrimSpace(value)
		if _, err := strconv.ParseInt(value, 10, 64); err != nil {
			return nil, fmt.Errorf("invalid int index %q", value)
		}
		return json.RawMessage([]byte(value)), nil
	case "string":
		b, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		return json.RawMessage(b), nil
	default:
		return nil, fmt.Errorf("unsupported index kind %q", kind)
	}
}

func compareStateEngineInstances(a, b tfstate.ResourceInstance) int {
	ak, av, _ := a.DecodeIndex()
	bk, bv, _ := b.DecodeIndex()
	if ak != bk {
		if ak < bk {
			return -1
		}
		return 1
	}
	switch ak {
	case tfstate.IndexInt:
		ai, _ := strconv.ParseInt(av, 10, 64)
		bi, _ := strconv.ParseInt(bv, 10, 64)
		switch {
		case ai < bi:
			return -1
		case ai > bi:
			return 1
		default:
			return 0
		}
	default:
		switch {
		case av < bv:
			return -1
		case av > bv:
			return 1
		default:
			return 0
		}
	}
}

func dedupeSortedStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, raw := range in {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
