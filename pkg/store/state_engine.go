package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
)

// StateEngineStateInfo is the minimal shared state metadata needed by the
// state-engine protocol.
type StateEngineStateInfo struct {
	StateID string `json:"state_id"`
	Lineage string `json:"lineage"`
	Serial  int64  `json:"serial"`
}

// StateEngineResource is the current/live view of a resource row enriched with
// dependency and hash metadata needed by the state-engine protocol.
type StateEngineResource struct {
	ResourceSnapshot
	Dependencies   []string `json:"dependencies,omitempty"`
	AttributesHash string   `json:"attributes_hash,omitempty"`
}

// StateEngineResourceInventory is the lean graph/inventory view used for
// backend-side scope expansion without materializing full resource payloads.
type StateEngineResourceInventory struct {
	Address        string `json:"address"`
	Mode           string `json:"mode"`
	Type           string `json:"type"`
	Name           string `json:"name"`
	Provider       string `json:"provider"`
	ModulePath     string `json:"module_path"`
	IndexKind      string `json:"index_kind"`
	IndexValue     string `json:"index_value,omitempty"`
	CreateSerial   int64  `json:"create_serial"`
	DeleteSerial   *int64 `json:"delete_serial,omitempty"`
	AttributesHash string `json:"attributes_hash,omitempty"`
}

type stateEngineGraphRow struct {
	Resource        StateEngineResourceInventory
	DependenciesRaw string
}

// ResolveStateEngineStateInfo ensures the state exists and returns enough
// metadata for state-engine endpoints to talk about the current trunk.
func (s *Store) ResolveStateEngineStateInfo(ctx context.Context, name string) (*StateEngineStateInfo, error) {
	where, args := s.stateByNameWhere(ctx, name)
	q := `
		SELECT s.id,
		       COALESCE(s.lineage::text, ''),
		       COALESCE(sv.serial, 0)
		FROM   states s
		LEFT JOIN state_versions sv ON sv.id = s.current_version_id
		WHERE  ` + where
	var (
		stateID string
		lineage string
		serial  int64
	)
	err := s.pool.QueryRow(ctx, q, args...).Scan(&stateID, &lineage, &serial)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrStateNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("resolve state-engine state info: %w", err)
	}
	return &StateEngineStateInfo{
		StateID: stateID,
		Lineage: lineage,
		Serial:  serial,
	}, nil
}

// ListCurrentResourceInventoryForStateEngine returns the current realized graph
// inventory without loading full resource attribute payloads.
func (s *Store) ListCurrentResourceInventoryForStateEngine(ctx context.Context, stateName string) ([]StateEngineResourceInventory, error) {
	where, args := s.stateByNameWhere(ctx, stateName)
	q := `
		SELECT r.address,
		       r.mode,
		       r.type,
		       r.name,
		       r.provider,
		       r.module_path,
		       r.index_kind,
		       COALESCE(r.index_value, ''),
		       r.create_serial,
		       r.delete_serial,
		       r.attributes_hash
		FROM   states s
		JOIN   resources r
		       ON r.state_id = s.id
		      AND r.delete_serial IS NULL
		WHERE  ` + where + `
		ORDER  BY r.address`
	rows, err := s.pool.Query(ctx, q, args...)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrStateNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("list current resources for state-engine: %w", err)
	}
	defer rows.Close()

	out := make([]StateEngineResourceInventory, 0)
	for rows.Next() {
		var row StateEngineResourceInventory
		if err := rows.Scan(
			&row.Address,
			&row.Mode,
			&row.Type,
			&row.Name,
			&row.Provider,
			&row.ModulePath,
			&row.IndexKind,
			&row.IndexValue,
			&row.CreateSerial,
			&row.DeleteSerial,
			&row.AttributesHash,
		); err != nil {
			return nil, fmt.Errorf("scan current state-engine inventory resource: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate current state-engine inventory resources: %w", err)
	}
	if len(out) == 0 {
		if _, _, err := s.lookupStateIDAndCurrent(ctx, stateName); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// LoadCurrentGraphSnapshotForStateEngine returns the current realized resource
// inventory plus dependency adjacency in one pass over the open resource rows.
// This avoids the much more expensive SQL view/self-join used by the generic
// current_resource_dependencies surface.
func (s *Store) LoadCurrentGraphSnapshotForStateEngine(ctx context.Context, stateName string) ([]StateEngineResourceInventory, map[string][]string, error) {
	where, args := s.stateByNameWhere(ctx, stateName)
	q := `
		SELECT r.address,
		       r.mode,
		       r.type,
		       r.name,
		       r.provider,
		       r.module_path,
		       r.index_kind,
		       COALESCE(r.index_value, ''),
		       r.create_serial,
		       r.delete_serial,
		       r.attributes_hash,
		       r.dependencies_raw::text
		FROM   states s
		JOIN   resources r
		       ON r.state_id = s.id
		      AND r.delete_serial IS NULL
		WHERE  ` + where + `
		ORDER  BY r.address`
	rows, err := s.pool.Query(ctx, q, args...)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, ErrStateNotFound
	}
	if err != nil {
		return nil, nil, fmt.Errorf("load current graph snapshot for state-engine: %w", err)
	}
	defer rows.Close()

	graphRows := make([]stateEngineGraphRow, 0)
	for rows.Next() {
		var row stateEngineGraphRow
		if err := rows.Scan(
			&row.Resource.Address,
			&row.Resource.Mode,
			&row.Resource.Type,
			&row.Resource.Name,
			&row.Resource.Provider,
			&row.Resource.ModulePath,
			&row.Resource.IndexKind,
			&row.Resource.IndexValue,
			&row.Resource.CreateSerial,
			&row.Resource.DeleteSerial,
			&row.Resource.AttributesHash,
			&row.DependenciesRaw,
		); err != nil {
			return nil, nil, fmt.Errorf("scan current graph snapshot row for state-engine: %w", err)
		}
		graphRows = append(graphRows, row)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterate current graph snapshot rows for state-engine: %w", err)
	}
	if len(graphRows) == 0 {
		if _, _, err := s.lookupStateIDAndCurrent(ctx, stateName); err != nil {
			return nil, nil, err
		}
		return []StateEngineResourceInventory{}, map[string][]string{}, nil
	}
	return buildStateEngineGraphSnapshot(graphRows)
}

// ListCurrentDependencyAdjacencyForStateEngine returns the realized dependency
// graph as address-to-address adjacency, without decoding dependency JSON in Go.
func (s *Store) ListCurrentDependencyAdjacencyForStateEngine(ctx context.Context, stateName string) (map[string][]string, error) {
	where, args := s.stateByNameWhere(ctx, stateName)
	q := `
		SELECT r_from.address,
		       r_to.address
		FROM   states s
		JOIN   resources r_from
		       ON r_from.state_id = s.id
		      AND r_from.delete_serial IS NULL
		JOIN   current_resource_dependencies d
		       ON d.from_resource_id = r_from.id
		JOIN   resources r_to
		       ON r_to.id = d.to_resource_id
		WHERE  ` + where + `
		ORDER  BY r_from.address, r_to.address`
	rows, err := s.pool.Query(ctx, q, args...)
	if errors.Is(err, pgx.ErrNoRows) {
		return map[string][]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list current dependency adjacency for state-engine: %w", err)
	}
	defer rows.Close()

	out := map[string][]string{}
	for rows.Next() {
		var fromAddr, toAddr string
		if err := rows.Scan(&fromAddr, &toAddr); err != nil {
			return nil, fmt.Errorf("scan current state-engine dependency adjacency: %w", err)
		}
		out[fromAddr] = append(out[fromAddr], toAddr)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate current state-engine dependency adjacency: %w", err)
	}
	for fromAddr := range out {
		slices.Sort(out[fromAddr])
	}
	return out, nil
}

func buildStateEngineGraphSnapshot(rows []stateEngineGraphRow) ([]StateEngineResourceInventory, map[string][]string, error) {
	resources := make([]StateEngineResourceInventory, 0, len(rows))
	exact := make(map[string]struct{}, len(rows))
	indexedByBase := make(map[string][]string)
	for _, row := range rows {
		resources = append(resources, row.Resource)
		exact[row.Resource.Address] = struct{}{}
		if base, ok := baseAddressForIndexedInstance(row.Resource.Address); ok {
			indexedByBase[base] = append(indexedByBase[base], row.Resource.Address)
		}
	}
	for base := range indexedByBase {
		slices.Sort(indexedByBase[base])
	}

	adjacency := make(map[string][]string, len(rows))
	for _, row := range rows {
		deps, err := decodeStateEngineDependencies(row.DependenciesRaw)
		if err != nil {
			return nil, nil, fmt.Errorf("decode state-engine dependencies for %s: %w", row.Resource.Address, err)
		}
		if len(deps) == 0 {
			continue
		}
		resolved := make([]string, 0, len(deps))
		seen := make(map[string]struct{}, len(deps))
		for _, dep := range deps {
			if _, ok := exact[dep]; ok {
				if _, dup := seen[dep]; !dup {
					seen[dep] = struct{}{}
					resolved = append(resolved, dep)
				}
			}
			for _, candidate := range indexedByBase[dep] {
				if _, dup := seen[candidate]; dup {
					continue
				}
				seen[candidate] = struct{}{}
				resolved = append(resolved, candidate)
			}
		}
		if len(resolved) == 0 {
			continue
		}
		slices.Sort(resolved)
		adjacency[row.Resource.Address] = resolved
	}
	return resources, adjacency, nil
}

func decodeStateEngineDependencies(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "[]" {
		return nil, nil
	}
	var deps []string
	if err := json.Unmarshal([]byte(raw), &deps); err != nil {
		return nil, err
	}
	return deps, nil
}

func baseAddressForIndexedInstance(addr string) (string, bool) {
	addr = strings.TrimSpace(addr)
	if !strings.HasSuffix(addr, "]") {
		return "", false
	}
	open := strings.LastIndex(addr, "[")
	if open <= 0 || open >= len(addr)-1 {
		return "", false
	}
	return addr[:open], true
}

// MaterializeCurrentResourcesForStateEngine loads the full current resource
// payloads only for the selected realized addresses.
func (s *Store) MaterializeCurrentResourcesForStateEngine(ctx context.Context, stateName string, addresses []string) ([]StateEngineResource, error) {
	addresses = dedupeTrimmedStrings(addresses)
	if len(addresses) == 0 {
		if _, _, err := s.lookupStateIDAndCurrent(ctx, stateName); err != nil {
			return nil, err
		}
		return []StateEngineResource{}, nil
	}
	where, args := s.stateByNameWhere(ctx, stateName)
	args = append(args, addresses)
	q := `
		SELECT r.address,
		       r.mode,
		       r.type,
		       r.name,
		       r.provider,
		       r.module_path,
		       r.index_kind,
		       COALESCE(r.index_value, ''),
		       r.create_serial,
		       r.delete_serial,
		       r.attributes,
		       r.sensitive_paths,
		       r.dependencies_raw::text,
		       r.attributes_hash
		FROM   states s
		JOIN   resources r
		       ON r.state_id = s.id
		      AND r.delete_serial IS NULL
		WHERE  ` + where + `
		  AND  r.address = ANY($` + fmt.Sprintf("%d", len(args)) + `)
		ORDER  BY r.address`
	rows, err := s.pool.Query(ctx, q, args...)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrStateNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("materialize current resources for state-engine: %w", err)
	}
	defer rows.Close()

	out := make([]StateEngineResource, 0, len(addresses))
	for rows.Next() {
		var (
			row             StateEngineResource
			dependenciesRaw string
		)
		if err := rows.Scan(
			&row.Address,
			&row.Mode,
			&row.Type,
			&row.Name,
			&row.Provider,
			&row.ModulePath,
			&row.IndexKind,
			&row.IndexValue,
			&row.CreateSerial,
			&row.DeleteSerial,
			&row.Attributes,
			&row.SensitivePaths,
			&dependenciesRaw,
			&row.AttributesHash,
		); err != nil {
			return nil, fmt.Errorf("scan materialized state-engine resource: %w", err)
		}
		if strings.TrimSpace(dependenciesRaw) != "" {
			if err := json.Unmarshal([]byte(dependenciesRaw), &row.Dependencies); err != nil {
				return nil, fmt.Errorf("decode materialized resource dependencies for %s: %w", row.Address, err)
			}
			slices.Sort(row.Dependencies)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate materialized state-engine resources: %w", err)
	}
	return out, nil
}

func dedupeTrimmedStrings(in []string) []string {
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
	slices.Sort(out)
	return out
}
