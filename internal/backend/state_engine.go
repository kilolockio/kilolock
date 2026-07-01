package backend

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/kilolockio/kilolock/pkg/auth"
	"github.com/kilolockio/kilolock/pkg/store"
)

func stateEngineAdminApplyID(kind string) string {
	kind = strings.ToLower(strings.TrimSpace(kind))
	kind = strings.ReplaceAll(kind, " ", "-")
	if kind == "" {
		kind = "admin"
	}
	return fmt.Sprintf("%s-%d", kind, time.Now().UTC().UnixNano())
}

func (s *Server) withStateEngineMutationLock(
	w http.ResponseWriter,
	r *http.Request,
	st *store.Store,
	stateName, kind, actor string,
	scopeSummary []string,
	fn func(),
) {
	applyID := stateEngineAdminApplyID(kind)
	current, err := st.AcquireStateEngineLock(r.Context(), stateName, applyID, actor, scopeSummary)
	if err != nil {
		if errors.Is(err, store.ErrAlreadyLocked) {
			writeJSON(w, http.StatusLocked, map[string]any{
				"error": "state locked",
				"lock":  current,
			})
			return
		}
		s.handleStateEngineStoreError(w, r, stateName, "acquire state-engine coarse lock", err)
		return
	}
	defer func() {
		if err := st.ReleaseStateEngineLock(r.Context(), stateName, applyID, actor); err != nil {
			s.logger.Error("release state-engine coarse lock failed",
				append(requestLogAttrs(r.Context(), stateName),
					"apply_id", applyID,
					"err", err,
				)...,
			)
		}
	}()
	fn()
}

type stateEngineSelector struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

type stateEngineClientContext struct {
	ExplicitWriteCandidates []string                `json:"explicit_write_candidates"`
	ExplicitReadCandidates  []string                `json:"explicit_read_candidates"`
	UndeployedCandidates    []string                `json:"undeployed_config_candidates"`
	RemovedCandidates       []string                `json:"removed_config_candidates"`
	ConfigNodes             []stateEngineConfigNode `json:"config_nodes"`
}

type stateEngineConfigNode struct {
	Address      string   `json:"address"`
	Dependencies []string `json:"dependencies"`
}

type stateEngineReservationCandidate struct {
	AddressGlob string `json:"address_glob"`
	Mode        string `json:"mode"`
}

type stateEngineScopeDiagnostics struct {
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
}

type stateEngineSliceDiagnostics struct {
	RequestedAddressCount int   `json:"requested_address_count"`
	MaterializedCount     int   `json:"materialized_count"`
	MaterializeDurationMs int64 `json:"materialize_duration_ms"`
}

type stateEngineScopeContract struct {
	FetchAddresses          []string                          `json:"fetch_addresses"`
	WriteAddresses          []string                          `json:"write_addresses"`
	ReadAddresses           []string                          `json:"read_addresses"`
	ConfigRequiredNodes     []string                          `json:"config_required_nodes"`
	RemovedConfigNodes      []string                          `json:"removed_config_nodes"`
	MissingFromState        []string                          `json:"missing_from_state"`
	UndeployedCandidates    []string                          `json:"undeployed_candidates"`
	UnknownMissingFromState []string                          `json:"unknown_missing_from_state"`
	ReservationCandidates   []stateEngineReservationCandidate `json:"reservation_candidates"`
	Confidence              string                            `json:"confidence"`
	Notes                   []string                          `json:"notes"`
	Diagnostics             stateEngineScopeDiagnostics       `json:"diagnostics"`
}

type stateEngineStateRequest struct {
	State       string `json:"state"`
	StateURL    string `json:"state_url"`
	WorkspaceID string `json:"workspace_id"`
	EnvPublicID string `json:"env_public_id"`
	StateName   string `json:"state_name"`
}

type stateEngineScopeExpandRequest struct {
	stateEngineStateRequest
	Selectors     []stateEngineSelector    `json:"selectors"`
	ClientContext stateEngineClientContext `json:"client_context"`
}

type stateEngineSliceRequest struct {
	stateEngineStateRequest
	Addresses  []string `json:"addresses"`
	BaseSerial int64    `json:"base_serial"`
}

type stateEngineCommitRequest struct {
	stateEngineStateRequest
	ApplyID    string                        `json:"apply_id"`
	BaseSerial int64                         `json:"base_serial"`
	Mode       string                        `json:"mode"`
	RawState   string                        `json:"raw_state"`
	WriteSet   []string                      `json:"write_set"`
	Delta      *store.StateEngineDeltaCommit `json:"delta"`
	Source     string                        `json:"source"`
	Actor      string                        `json:"actor"`
}

type stateEngineCommitResponse struct {
	OK              bool   `json:"ok"`
	CommittedSerial int64  `json:"committed_serial"`
	NewVersionID    string `json:"new_version_id"`
	CommitMode      string `json:"commit_mode,omitempty"`
}

type stateEngineLockAcquireRequest struct {
	stateEngineStateRequest
	ApplyID      string   `json:"apply_id"`
	Holder       string   `json:"holder"`
	ScopeSummary []string `json:"scope_summary"`
}

type stateEngineLockReleaseRequest struct {
	stateEngineStateRequest
	ApplyID string `json:"apply_id"`
	Actor   string `json:"actor"`
}

type stateEngineResourceMutationRequest struct {
	stateEngineStateRequest
	Address string `json:"address"`
	To      string `json:"to"`
	Actor   string `json:"actor"`
}

type stateEngineRollbackResponse struct {
	OK      bool                           `json:"ok"`
	Preview *store.ResourceRollbackPreview `json:"preview,omitempty"`
	Version *store.StateVersionInfo        `json:"version,omitempty"`
}

type stateEngineReservationAcquireRequest struct {
	stateEngineStateRequest
	StateID      string              `json:"state_id"`
	ApplyID      string              `json:"apply_id"`
	Holder       string              `json:"holder"`
	LeaseSeconds int                 `json:"lease_seconds"`
	Want         []store.Reservation `json:"want"`
}

type stateEngineReservationLeaseRequest struct {
	LeaseSeconds int `json:"lease_seconds"`
}

type stateEngineApplyRunBeginRequest struct {
	stateEngineStateRequest
	StateID       string          `json:"state_id"`
	FromVersionID string          `json:"from_version_id"`
	Actor         string          `json:"actor"`
	SourceSerial  int64           `json:"source_serial"`
	Info          json.RawMessage `json:"info"`
}

func (s *Server) handleStateEngineCapabilities(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"protocol": "state-engine",
		"version":  "v1",
		"capabilities": map[string]bool{
			"slice_fetch":                   true,
			"backend_closure_expansion":     true,
			"resource_reservations":         true,
			"terraform_visible_native_lock": true,
			"delta_commit":                  true,
			"native_state_rm":               true,
			"native_state_mv":               true,
			"native_resource_rollback":      true,
		},
	})
}

func (s *Server) handleStateEngineResolve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	st, name, principal, ok := s.resolveStateEngineStoreAndName(w, r)
	if !ok {
		return
	}
	info, err := st.ResolveStateEngineStateInfo(r.Context(), name)
	if err != nil {
		s.handleStateEngineStoreError(w, r, name, "resolve state-engine metadata", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"state":         name,
		"state_id":      info.StateID,
		"workspace_id":  principal.WorkspaceID,
		"env_public_id": principal.EnvironmentPublicID,
		"state_name":    baseStateName(name),
		"lineage":       info.Lineage,
		"serial":        info.Serial,
		"creator":       "state_engine",
	})
}

func (s *Server) handleStateEngineScopeExpand(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	st, name, _, body, ok := s.resolveStateEngineStoreNameAndBody(w, r)
	if !ok {
		return
	}
	var in stateEngineScopeExpandRequest
	if err := json.Unmarshal(body, &in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "request body must be valid JSON")
		return
	}
	expandStarted := time.Now()
	_, snapshot, graphCacheHit, err := s.loadStateEngineGraphSnapshot(r.Context(), st, name)
	if err != nil {
		s.handleStateEngineStoreError(w, r, name, "expand state-engine scope", err)
		return
	}
	resources := snapshot.Resources
	dependencyAdjacency := snapshot.DependencyAdjacency

	resourceByAddress := make(map[string]store.StateEngineResourceInventory, len(resources))
	for _, resource := range resources {
		resourceByAddress[resource.Address] = resource
	}
	configGraph := map[string][]string{}
	for _, node := range in.ClientContext.ConfigNodes {
		addr := strings.TrimSpace(node.Address)
		if addr == "" {
			continue
		}
		configGraph[addr] = dedupeSortedStrings(node.Dependencies)
	}
	selectedWrite := map[string]struct{}{}
	inventoryScanCount := 0
	moduleSelectors := make([]string, 0, len(in.Selectors))
	for _, selector := range in.Selectors {
		switch strings.TrimSpace(selector.Kind) {
		case "resource_address":
			addr := strings.TrimSpace(selector.Value)
			if _, ok := resourceByAddress[addr]; ok {
				selectedWrite[addr] = struct{}{}
			}
		case "module_prefix":
			if prefix := strings.TrimSpace(selector.Value); prefix != "" {
				moduleSelectors = append(moduleSelectors, prefix)
			}
		}
	}
	if len(moduleSelectors) > 0 {
		moduleSelectors = dedupeSortedStrings(moduleSelectors)
		for _, prefix := range moduleSelectors {
			members := snapshot.ModuleMembers[prefix]
			if len(members) == 0 {
				continue
			}
			for _, addr := range members {
				selectedWrite[addr] = struct{}{}
			}
		}
	}
	for _, addr := range in.ClientContext.ExplicitWriteCandidates {
		if _, ok := resourceByAddress[addr]; ok {
			selectedWrite[addr] = struct{}{}
		}
	}
	desiredWrite := map[string]struct{}{}
	for _, addr := range in.ClientContext.ExplicitWriteCandidates {
		addr = strings.TrimSpace(addr)
		if addr != "" {
			desiredWrite[addr] = struct{}{}
		}
	}
	desiredRead := map[string]struct{}{}
	for _, addr := range in.ClientContext.ExplicitReadCandidates {
		addr = strings.TrimSpace(addr)
		if addr != "" {
			desiredRead[addr] = struct{}{}
		}
	}
	readClosure := map[string]struct{}{}
	requiredConfig := map[string]struct{}{}
	unresolvedDeps := map[string]struct{}{}
	queueSet := map[string]struct{}{}
	queue := make([]string, 0, len(selectedWrite)+len(desiredWrite)+len(desiredRead))
	push := func(addr string) {
		addr = strings.TrimSpace(addr)
		if addr == "" {
			return
		}
		if _, ok := queueSet[addr]; ok {
			return
		}
		queueSet[addr] = struct{}{}
		queue = append(queue, addr)
	}
	for addr := range selectedWrite {
		push(addr)
	}
	for addr := range desiredWrite {
		push(addr)
	}
	for addr := range desiredRead {
		push(addr)
	}
	visited := map[string]struct{}{}
	for len(queue) > 0 {
		addr := queue[0]
		queue = queue[1:]
		if _, ok := visited[addr]; ok {
			continue
		}
		visited[addr] = struct{}{}
		if resource, ok := resourceByAddress[addr]; ok {
			deps := append([]string{}, dependencyAdjacency[resource.Address]...)
			deps = append(deps, configGraph[addr]...)
			for _, dep := range dedupeSortedStrings(deps) {
				if _, ok := resourceByAddress[dep]; ok {
					push(dep)
					continue
				}
				if _, ok := configGraph[dep]; ok {
					push(dep)
					continue
				}
				unresolvedDeps[dep] = struct{}{}
			}
			continue
		}
		deps, ok := configGraph[addr]
		if !ok {
			continue
		}
		requiredConfig[addr] = struct{}{}
		for _, dep := range deps {
			if _, ok := resourceByAddress[dep]; ok {
				push(dep)
				continue
			}
			if _, ok := configGraph[dep]; ok {
				push(dep)
				continue
			}
			unresolvedDeps[dep] = struct{}{}
		}
	}
	for addr := range visited {
		if _, ok := resourceByAddress[addr]; !ok {
			continue
		}
		if _, ok := selectedWrite[addr]; ok {
			continue
		}
		readClosure[addr] = struct{}{}
	}

	missing := map[string]struct{}{}
	for _, addr := range append(append([]string{}, in.ClientContext.ExplicitWriteCandidates...), in.ClientContext.ExplicitReadCandidates...) {
		addr = strings.TrimSpace(addr)
		if addr == "" {
			continue
		}
		if _, ok := resourceByAddress[addr]; !ok {
			missing[addr] = struct{}{}
		}
	}
	for _, selector := range in.Selectors {
		if selector.Kind != "resource_address" {
			continue
		}
		addr := strings.TrimSpace(selector.Value)
		if addr == "" {
			continue
		}
		if _, ok := resourceByAddress[addr]; !ok {
			missing[addr] = struct{}{}
		}
	}
	undeployed := make(map[string]struct{}, len(in.ClientContext.UndeployedCandidates))
	for _, addr := range in.ClientContext.UndeployedCandidates {
		addr = strings.TrimSpace(addr)
		if addr != "" {
			undeployed[addr] = struct{}{}
		}
	}
	removed := make(map[string]struct{}, len(in.ClientContext.RemovedCandidates))
	for _, addr := range in.ClientContext.RemovedCandidates {
		addr = strings.TrimSpace(addr)
		if addr != "" {
			removed[addr] = struct{}{}
		}
	}
	undeployedMissing := map[string]struct{}{}
	removedAlreadyAbsent := map[string]struct{}{}
	unknownMissing := map[string]struct{}{}
	confidence := "safe"
	notes := make([]string, 0, 4)
	for addr := range missing {
		if _, ok := removed[addr]; ok {
			removedAlreadyAbsent[addr] = struct{}{}
			continue
		}
		if _, ok := undeployed[addr]; !ok {
			unknownMissing[addr] = struct{}{}
			confidence = "unsafe"
			continue
		}
		undeployedMissing[addr] = struct{}{}
	}
	for addr := range unresolvedDeps {
		if _, ok := undeployed[addr]; ok {
			undeployedMissing[addr] = struct{}{}
			continue
		}
		unknownMissing[addr] = struct{}{}
		confidence = "unsafe"
	}

	writeSet := sortedSetKeys(selectedWrite)
	readSet := sortedSetKeys(readClosure)
	fetchSet := dedupeSortedStrings(append(append([]string{}, writeSet...), readSet...))
	configRequiredSet := sortedSetKeys(requiredConfig)
	removedConfigSet := sortedSetKeys(removed)
	missingSet := sortedSetKeys(missing)
	undeployedSet := sortedSetKeys(undeployedMissing)
	unknownSet := sortedSetKeys(unknownMissing)
	removedRealizedCount := 0
	for addr := range removed {
		if _, ok := resourceByAddress[addr]; ok {
			removedRealizedCount++
		}
	}
	if n := len(configRequiredSet); n > 0 {
		notes = append(notes, fmt.Sprintf("%d config-only node(s) are required for local planning", n))
	}
	if removedRealizedCount > 0 {
		notes = append(notes, fmt.Sprintf("%d removed config node(s) still exist in realized state and must be deleted", removedRealizedCount))
	}
	if n := len(removedAlreadyAbsent); n > 0 {
		notes = append(notes, fmt.Sprintf("%d removed config node(s) were already absent from realized state", n))
	}
	reservations := make([]stateEngineReservationCandidate, 0, len(writeSet)+len(readSet))
	for _, addr := range writeSet {
		reservations = append(reservations, stateEngineReservationCandidate{AddressGlob: addr, Mode: string(store.ReservationWrite)})
	}
	for _, addr := range readSet {
		reservations = append(reservations, stateEngineReservationCandidate{AddressGlob: addr, Mode: string(store.ReservationRead)})
	}
	diagnostics := stateEngineScopeDiagnostics{
		GraphCacheHit:         graphCacheHit,
		RealizedResourceCount: len(resources),
		DependencyEdgeCount:   countStateEngineAdjacencyEdges(dependencyAdjacency),
		InventoryScanCount:    inventoryScanCount,
		WalkedNodeCount:       len(visited),
		ConfigNodeCount:       len(configGraph),
		ModuleSelectorCount:   len(moduleSelectors),
		FetchAddressCount:     len(fetchSet),
		WriteAddressCount:     len(writeSet),
		ReadAddressCount:      len(readSet),
		ExpandDurationMs:      time.Since(expandStarted).Milliseconds(),
	}
	scopeContract := stateEngineScopeContract{
		FetchAddresses:          fetchSet,
		WriteAddresses:          writeSet,
		ReadAddresses:           readSet,
		ConfigRequiredNodes:     configRequiredSet,
		RemovedConfigNodes:      removedConfigSet,
		MissingFromState:        missingSet,
		UndeployedCandidates:    undeployedSet,
		UnknownMissingFromState: unknownSet,
		ReservationCandidates:   reservations,
		Confidence:              confidence,
		Notes:                   notes,
		Diagnostics:             diagnostics,
	}
	s.logger.Debug("state-engine scope expanded",
		append(requestLogAttrs(r.Context(), name),
			"graph_cache_hit", diagnostics.GraphCacheHit,
			"realized_resource_count", diagnostics.RealizedResourceCount,
			"dependency_edge_count", diagnostics.DependencyEdgeCount,
			"inventory_scan_count", diagnostics.InventoryScanCount,
			"walked_node_count", diagnostics.WalkedNodeCount,
			"config_node_count", diagnostics.ConfigNodeCount,
			"module_selector_count", diagnostics.ModuleSelectorCount,
			"fetch_address_count", diagnostics.FetchAddressCount,
			"write_address_count", diagnostics.WriteAddressCount,
			"read_address_count", diagnostics.ReadAddressCount,
			"duration_ms", diagnostics.ExpandDurationMs,
		)...,
	)
	writeJSON(w, http.StatusOK, map[string]any{
		"state":                      name,
		"scope_contract":             scopeContract,
		"contract_source":            "backend_authoritative",
		"fetch_addresses":            fetchSet,
		"realized_write_closure":     writeSet,
		"realized_read_closure":      readSet,
		"missing_from_state":         missingSet,
		"undeployed_candidates":      undeployedSet,
		"unknown_missing_from_state": unknownSet,
		"reservation_candidates":     reservations,
		"confidence":                 confidence,
		"notes":                      notes,
		"diagnostics":                diagnostics,
	})
}

func (s *Server) handleStateEngineSlice(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	st, name, _, body, ok := s.resolveStateEngineStoreNameAndBody(w, r)
	if !ok {
		return
	}
	var in stateEngineSliceRequest
	if err := json.Unmarshal(body, &in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "request body must be valid JSON")
		return
	}
	info, err := st.ResolveStateEngineStateInfo(r.Context(), name)
	if err != nil {
		s.handleStateEngineStoreError(w, r, name, "resolve state-engine slice metadata", err)
		return
	}
	selected := make(map[string]struct{}, len(in.Addresses))
	for _, addr := range in.Addresses {
		addr = strings.TrimSpace(addr)
		if addr != "" {
			selected[addr] = struct{}{}
		}
	}
	materializeStarted := time.Now()
	out, err := st.MaterializeCurrentResourcesForStateEngine(r.Context(), name, sortedSetKeys(selected))
	if err != nil {
		s.handleStateEngineStoreError(w, r, name, "load state-engine slice", err)
		return
	}
	diagnostics := stateEngineSliceDiagnostics{
		RequestedAddressCount: len(selected),
		MaterializedCount:     len(out),
		MaterializeDurationMs: time.Since(materializeStarted).Milliseconds(),
	}
	s.logger.Debug("state-engine slice materialized",
		append(requestLogAttrs(r.Context(), name),
			"requested_address_count", diagnostics.RequestedAddressCount,
			"materialized_count", diagnostics.MaterializedCount,
			"duration_ms", diagnostics.MaterializeDurationMs,
		)...,
	)
	writeJSON(w, http.StatusOK, map[string]any{
		"state":    name,
		"state_id": info.StateID,
		"lineage":  info.Lineage,
		"serial":   info.Serial,
		"slice": map[string]any{
			"resources": out,
			"outputs":   []any{},
			"metadata": map[string]any{
				"base_serial": in.BaseSerial,
				"diagnostics": diagnostics,
			},
		},
	})
}

func (s *Server) handleStateEngineCommit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	st, name, _, body, ok := s.resolveStateEngineStoreNameAndBody(w, r)
	if !ok {
		return
	}
	var in stateEngineCommitRequest
	if err := json.Unmarshal(body, &in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "request body must be valid JSON")
		return
	}
	mode := strings.ToLower(strings.TrimSpace(in.Mode))
	if mode == "" {
		mode = "snapshot"
	}
	if mode != "snapshot" && mode != "delta" {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{
			"error":   "native_commit_not_supported",
			"message": fmt.Sprintf("state-engine commit mode %q is not supported in this build", mode),
		})
		return
	}
	if mode == "delta" && len(in.WriteSet) == 0 && in.Delta != nil {
		in.WriteSet = append([]string(nil), in.Delta.WriteSet...)
		in.WriteSet = append(in.WriteSet, in.Delta.DeleteSet...)
	}
	if mode == "delta" && len(in.WriteSet) == 0 && in.Delta == nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{
			"error":   "invalid_request",
			"message": `state-engine commit mode "delta" requires either delta.write_set or legacy write_set`,
		})
		return
	}

	source := firstNonEmptyString(strings.TrimSpace(in.Source), "state-engine-apply")
	actor := firstNonEmptyString(strings.TrimSpace(in.Actor), actorFromRequest(r))
	var err error
	commitMode := mode
	if mode == "delta" {
		switch {
		case in.Delta != nil:
			if len(in.Delta.WriteSet) == 0 {
				in.Delta.WriteSet = append([]string(nil), in.WriteSet...)
			}
			err = st.WriteStateEngineDeltaForApply(r.Context(), name, strings.TrimSpace(in.ApplyID), in.BaseSerial, *in.Delta, source, actor)
		default:
			err = st.WriteStateDeltaForApply(r.Context(), name, strings.TrimSpace(in.ApplyID), in.BaseSerial, []byte(in.RawState), source, actor, in.WriteSet)
		}
	} else {
		if len(in.WriteSet) > 0 {
			err = st.WriteStateDeltaForApply(r.Context(), name, strings.TrimSpace(in.ApplyID), in.BaseSerial, []byte(in.RawState), source, actor, in.WriteSet)
			commitMode = "snapshot-selected"
		} else {
			err = st.WriteStateForApply(r.Context(), name, strings.TrimSpace(in.ApplyID), in.BaseSerial, []byte(in.RawState), source, actor)
		}
	}
	if err != nil {
		switch {
		case errors.Is(err, store.ErrStateNotFound):
			writeJSONError(w, http.StatusNotFound, "state not found")
		case errors.Is(err, store.ErrSerialConflict):
			currentInfo, cerr := st.EnsureCurrentStateInfo(r.Context(), name)
			if cerr == nil {
				writeJSONStatus(w, http.StatusConflict, map[string]any{
					"error":          "state_serial_conflict",
					"current_serial": currentInfo.Serial,
				})
				return
			}
			writeJSONStatus(w, http.StatusConflict, map[string]any{"error": "state_serial_conflict"})
		default:
			s.handleStateEngineStoreError(w, r, name, "commit state-engine snapshot", err)
		}
		return
	}

	currentInfo, err := st.EnsureCurrentStateInfo(r.Context(), name)
	if err != nil {
		s.handleStateEngineStoreError(w, r, name, "resolve state-engine committed version", err)
		return
	}
	writeJSON(w, http.StatusOK, stateEngineCommitResponse{
		OK:              true,
		CommittedSerial: currentInfo.Serial,
		NewVersionID:    currentInfo.VersionID,
		CommitMode:      commitMode,
	})
}

func (s *Server) handleStateEngineReservationsAcquire(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	st, name, _, body, ok := s.resolveStateEngineStoreNameAndBody(w, r)
	if !ok {
		return
	}
	var in stateEngineReservationAcquireRequest
	if err := json.Unmarshal(body, &in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "request body must be valid JSON")
		return
	}
	info, err := st.ResolveStateEngineStateInfo(r.Context(), name)
	if err != nil {
		s.handleStateEngineStoreError(w, r, name, "resolve state-engine reservation state", err)
		return
	}
	leaseSeconds := in.LeaseSeconds
	if leaseSeconds <= 0 {
		leaseSeconds = 900
	}
	err = st.AcquireReservations(r.Context(), info.StateID, strings.TrimSpace(in.ApplyID), strings.TrimSpace(in.Holder), in.Want, time.Duration(leaseSeconds)*time.Second)
	var conflict *store.ReservationConflictError
	switch {
	case errors.As(err, &conflict):
		writeJSONStatus(w, http.StatusConflict, map[string]any{"error": "reservation_conflict", "conflicts": conflict.Conflicts})
		return
	case err != nil:
		s.handleStateEngineStoreError(w, r, name, "acquire state-engine reservations", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleStateEngineReservationsRenew(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	st, err := s.dataStore(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "environment unavailable")
		return
	}
	applyID := strings.TrimSpace(r.PathValue("apply_id"))
	if applyID == "" {
		writeJSONError(w, http.StatusBadRequest, "apply_id is required")
		return
	}
	body, err := readBody(r, maxLockBodyBytes)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	var in stateEngineReservationLeaseRequest
	if len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, &in); err != nil {
			writeJSONError(w, http.StatusBadRequest, "request body must be valid JSON")
			return
		}
	}
	leaseSeconds := in.LeaseSeconds
	if leaseSeconds <= 0 {
		leaseSeconds = 900
	}
	rows, err := st.RenewReservations(r.Context(), applyID, time.Duration(leaseSeconds)*time.Second)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "renewed": rows})
}

func (s *Server) handleStateEngineReservationsRelease(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	st, err := s.dataStore(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "environment unavailable")
		return
	}
	applyID := strings.TrimSpace(r.PathValue("apply_id"))
	if applyID == "" {
		writeJSONError(w, http.StatusBadRequest, "apply_id is required")
		return
	}
	if err := st.ReleaseReservations(r.Context(), applyID); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleStateEngineApplyRunBegin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	st, name, _, body, ok := s.resolveStateEngineStoreNameAndBody(w, r)
	if !ok {
		return
	}
	var in stateEngineApplyRunBeginRequest
	if err := json.Unmarshal(body, &in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "request body must be valid JSON")
		return
	}
	stateID := strings.TrimSpace(in.StateID)
	if stateID == "" {
		info, err := st.ResolveStateEngineStateInfo(r.Context(), name)
		if err != nil {
			s.handleStateEngineStoreError(w, r, name, "resolve state-engine apply state", err)
			return
		}
		stateID = info.StateID
	}
	run, err := st.BeginApplyRun(r.Context(), stateID, in.FromVersionID, in.Actor, in.SourceSerial, in.Info)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) handleStateEngineApplyRunStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	applyID := strings.TrimSpace(r.PathValue("id"))
	if applyID == "" {
		writeJSONError(w, http.StatusBadRequest, "apply id is required")
		return
	}
	st, err := s.dataStore(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "environment unavailable")
		return
	}
	status, err := st.GetApplyRunStatus(r.Context(), applyID)
	if errors.Is(err, store.ErrApplyRunNotFound) {
		writeJSONError(w, http.StatusNotFound, "apply run not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": status})
}

func (s *Server) handleStateEngineApplyRunFinish(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	applyID := strings.TrimSpace(r.PathValue("id"))
	if applyID == "" {
		writeJSONError(w, http.StatusBadRequest, "apply id is required")
		return
	}
	var in store.FinishApplyRunInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	st, err := s.dataStore(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "environment unavailable")
		return
	}
	if err := st.FinishApplyRun(r.Context(), applyID, in); err != nil {
		switch {
		case errors.Is(err, store.ErrApplyRunNotFound):
			writeJSONError(w, http.StatusNotFound, "apply run not found")
		default:
			writeJSONError(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleStateEngineApplyRunAbort(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	applyID := strings.TrimSpace(r.PathValue("id"))
	if applyID == "" {
		writeJSONError(w, http.StatusBadRequest, "apply id is required")
		return
	}
	var in struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	st, err := s.dataStore(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "environment unavailable")
		return
	}
	if err := st.AbortApplyRun(r.Context(), applyID, in.Reason); err != nil {
		switch {
		case errors.Is(err, store.ErrApplyRunNotFound):
			writeJSONError(w, http.StatusNotFound, "apply run not found")
		default:
			writeJSONError(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleStateEngineTerraformLockAcquire(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	st, name, _, body, ok := s.resolveStateEngineStoreNameAndBody(w, r)
	if !ok {
		return
	}
	var in stateEngineLockAcquireRequest
	if err := json.Unmarshal(body, &in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "request body must be valid JSON")
		return
	}
	current, err := st.AcquireStateEngineLock(r.Context(), name, in.ApplyID, in.Holder, in.ScopeSummary)
	var tenantInactive *store.TenantNotActiveError
	switch {
	case errors.Is(err, store.ErrAlreadyLocked):
		writeJSONStatus(w, http.StatusLocked, current)
		return
	case errors.As(err, &tenantInactive):
		writeJSONError(w, http.StatusForbidden, tenantInactive.Error())
		return
	case err != nil:
		s.handleStateEngineStoreError(w, r, name, "acquire state-engine coarse lock", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"lock_id": "state-engine-" + strings.TrimSpace(in.ApplyID),
	})
}

func (s *Server) handleStateEngineTerraformLockRelease(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	st, name, _, body, ok := s.resolveStateEngineStoreNameAndBody(w, r)
	if !ok {
		return
	}
	var in stateEngineLockReleaseRequest
	if err := json.Unmarshal(body, &in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "request body must be valid JSON")
		return
	}
	if err := st.ReleaseStateEngineLock(r.Context(), name, in.ApplyID, firstNonEmptyString(strings.TrimSpace(in.Actor), actorFromRequest(r))); err != nil {
		s.handleStateEngineStoreError(w, r, name, "release state-engine coarse lock", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleStateEngineResourceRemovePreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	st, name, _, body, ok := s.resolveStateEngineStoreNameAndBody(w, r)
	if !ok {
		return
	}
	var in stateEngineResourceMutationRequest
	if err := json.Unmarshal(body, &in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "request body must be valid JSON")
		return
	}
	preview, err := st.PreviewRemoveResourceCurrent(r.Context(), name, in.Address)
	if err != nil {
		s.handleStateEngineStoreError(w, r, name, "preview native resource remove", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "preview": preview})
}

func (s *Server) handleStateEngineResourceRemoveApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	st, name, _, body, ok := s.resolveStateEngineStoreNameAndBody(w, r)
	if !ok {
		return
	}
	var in stateEngineResourceMutationRequest
	if err := json.Unmarshal(body, &in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "request body must be valid JSON")
		return
	}
	actor := firstNonEmptyString(strings.TrimSpace(in.Actor), actorFromRequest(r))
	s.withStateEngineMutationLock(w, r, st, name, "state-engine-resource-remove", actor, []string{strings.TrimSpace(in.Address)}, func() {
		version, preview, err := st.ApplyRemoveResourceCurrent(r.Context(), name, in.Address, actor)
		if err != nil {
			s.handleStateEngineStoreError(w, r, name, "apply native resource remove", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "preview": preview, "version": version})
	})
}

func (s *Server) handleStateEngineResourceMovePreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	st, name, _, body, ok := s.resolveStateEngineStoreNameAndBody(w, r)
	if !ok {
		return
	}
	var in stateEngineResourceMutationRequest
	if err := json.Unmarshal(body, &in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "request body must be valid JSON")
		return
	}
	preview, err := st.PreviewMoveResourceCurrent(r.Context(), name, in.Address, in.To)
	if err != nil {
		s.handleStateEngineStoreError(w, r, name, "preview native resource move", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "preview": preview})
}

func (s *Server) handleStateEngineResourceMoveApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	st, name, _, body, ok := s.resolveStateEngineStoreNameAndBody(w, r)
	if !ok {
		return
	}
	var in stateEngineResourceMutationRequest
	if err := json.Unmarshal(body, &in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "request body must be valid JSON")
		return
	}
	actor := firstNonEmptyString(strings.TrimSpace(in.Actor), actorFromRequest(r))
	scope := []string{strings.TrimSpace(in.Address)}
	if to := strings.TrimSpace(in.To); to != "" {
		scope = append(scope, to)
	}
	s.withStateEngineMutationLock(w, r, st, name, "state-engine-resource-move", actor, scope, func() {
		version, preview, err := st.ApplyMoveResourceCurrent(r.Context(), name, in.Address, in.To, actor)
		if err != nil {
			s.handleStateEngineStoreError(w, r, name, "apply native resource move", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "preview": preview, "version": version})
	})
}

func (s *Server) handleStateEngineResourceRollbackPreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	st, name, _, body, ok := s.resolveStateEngineStoreNameAndBody(w, r)
	if !ok {
		return
	}
	var in stateEngineResourceMutationRequest
	if err := json.Unmarshal(body, &in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "request body must be valid JSON")
		return
	}
	preview, err := st.PreviewReplayResourceVersion(r.Context(), name, in.Address, in.To)
	if err != nil {
		s.handleStateEngineStoreError(w, r, name, "preview native resource rollback", err)
		return
	}
	writeJSON(w, http.StatusOK, stateEngineRollbackResponse{OK: true, Preview: preview})
}

func (s *Server) handleStateEngineResourceRollbackApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	st, name, _, body, ok := s.resolveStateEngineStoreNameAndBody(w, r)
	if !ok {
		return
	}
	var in stateEngineResourceMutationRequest
	if err := json.Unmarshal(body, &in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "request body must be valid JSON")
		return
	}
	actor := firstNonEmptyString(strings.TrimSpace(in.Actor), actorFromRequest(r))
	s.withStateEngineMutationLock(w, r, st, name, "state-engine-resource-rollback", actor, []string{strings.TrimSpace(in.Address), "to=" + strings.TrimSpace(in.To)}, func() {
		version, preview, err := st.ReplayResourceVersion(r.Context(), name, in.Address, in.To, actor)
		if err != nil {
			s.handleStateEngineStoreError(w, r, name, "apply native resource rollback", err)
			return
		}
		writeJSON(w, http.StatusOK, stateEngineRollbackResponse{OK: true, Preview: preview, Version: version})
	})
}

func (s *Server) resolveStateEngineStoreAndName(w http.ResponseWriter, r *http.Request) (*store.Store, string, auth.Principal, bool) {
	st, err := s.dataStore(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "environment unavailable")
		return nil, "", auth.Principal{}, false
	}
	body, err := readBody(r, maxStateBodyBytes)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return nil, "", auth.Principal{}, false
	}
	var in stateEngineStateRequest
	if len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, &in); err != nil {
			writeJSONError(w, http.StatusBadRequest, "request body must be valid JSON")
			return nil, "", auth.Principal{}, false
		}
	}
	name, err := resolveStateEngineName(in, auth.MustFromContext(r.Context()))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return nil, "", auth.Principal{}, false
	}
	return st, name, auth.MustFromContext(r.Context()), true
}

func (s *Server) resolveStateEngineStoreNameAndBody(w http.ResponseWriter, r *http.Request) (*store.Store, string, auth.Principal, []byte, bool) {
	st, err := s.dataStore(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "environment unavailable")
		return nil, "", auth.Principal{}, nil, false
	}
	body, err := readBody(r, maxStateBodyBytes)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return nil, "", auth.Principal{}, nil, false
	}
	var in stateEngineStateRequest
	if len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, &in); err != nil {
			writeJSONError(w, http.StatusBadRequest, "request body must be valid JSON")
			return nil, "", auth.Principal{}, nil, false
		}
	}
	principal := auth.MustFromContext(r.Context())
	name, err := resolveStateEngineName(in, principal)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return nil, "", auth.Principal{}, nil, false
	}
	return st, name, principal, body, true
}

func (s *Server) handleStateEngineStoreError(w http.ResponseWriter, r *http.Request, name, msg string, err error) {
	var tenantInactive *store.TenantNotActiveError
	switch {
	case errors.Is(err, store.ErrStateNotFound):
		writeJSONStatus(w, http.StatusNotFound, map[string]any{"error": "state_not_found"})
	case errors.As(err, &tenantInactive):
		writeJSONError(w, http.StatusForbidden, tenantInactive.Error())
	case isEnvironmentUnavailableError(err):
		writeJSONError(w, http.StatusServiceUnavailable, "environment unavailable")
	default:
		s.logger.Error(msg, append(requestLogAttrs(r.Context(), name), "err", err)...)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
	}
}

func resolveStateEngineName(in stateEngineStateRequest, principal auth.Principal) (string, error) {
	if state := strings.TrimSpace(in.State); state != "" {
		return state, nil
	}
	if stateURL := strings.TrimSpace(in.StateURL); stateURL != "" {
		u, err := url.Parse(stateURL)
		if err != nil {
			return "", fmt.Errorf("parse state_url: %w", err)
		}
		p := strings.TrimRight(u.Path, "/")
		i := strings.Index(p, "/v1/states/")
		if i < 0 {
			return "", fmt.Errorf("state_url must include /v1/states/")
		}
		name := strings.Trim(strings.TrimPrefix(p[i:], "/v1/states/"), "/")
		if name == "" {
			return "", fmt.Errorf("state_url must include a state name")
		}
		return name, nil
	}
	if strings.TrimSpace(in.WorkspaceID) != "" && strings.TrimSpace(in.EnvPublicID) != "" && strings.TrimSpace(in.StateName) != "" {
		return strings.TrimSpace(in.WorkspaceID) + "/" + strings.TrimSpace(in.EnvPublicID) + "/" + strings.TrimSpace(in.StateName), nil
	}
	if strings.TrimSpace(in.StateName) != "" && strings.TrimSpace(principal.WorkspaceID) != "" && strings.TrimSpace(principal.EnvironmentPublicID) != "" {
		return strings.TrimSpace(principal.WorkspaceID) + "/" + strings.TrimSpace(principal.EnvironmentPublicID) + "/" + strings.TrimSpace(in.StateName), nil
	}
	return "", fmt.Errorf("state, state_url, or workspace_id/env_public_id/state_name is required")
}

func sortedSetKeys(in map[string]struct{}) []string {
	out := make([]string, 0, len(in))
	for key := range in {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func dedupeSortedStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
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

func baseStateName(state string) string {
	state = strings.TrimSpace(state)
	if state == "" {
		return ""
	}
	parts := strings.Split(state, "/")
	return parts[len(parts)-1]
}
