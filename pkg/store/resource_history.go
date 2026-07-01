package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kilolockio/kilolock/internal/tfstate"
	"github.com/kilolockio/kilolock/pkg/auth"
)

// ResourceSnapshot is the current/live view of one Terraform resource instance
// in a state.
type ResourceSnapshot struct {
	Address        string          `json:"address"`
	Mode           string          `json:"mode"`
	Type           string          `json:"type"`
	Name           string          `json:"name"`
	Provider       string          `json:"provider"`
	ModulePath     string          `json:"module_path"`
	IndexKind      string          `json:"index_kind"`
	IndexValue     string          `json:"index_value,omitempty"`
	CreateSerial   int64           `json:"create_serial"`
	DeleteSerial   *int64          `json:"delete_serial,omitempty"`
	Attributes     json.RawMessage `json:"attributes,omitempty"`
	SensitivePaths json.RawMessage `json:"sensitive_paths,omitempty"`
}

// ResourceHistoryEntry describes one lifecycle span of a resource address across
// state versions.
type ResourceHistoryEntry struct {
	Address          string    `json:"address"`
	Mode             string    `json:"mode"`
	Type             string    `json:"type"`
	Provider         string    `json:"provider"`
	CreateSerial     int64     `json:"create_serial"`
	CreateVersionID  string    `json:"create_version_id"`
	CreateVersionAt  time.Time `json:"create_version_at"`
	CreateVersionBy  string    `json:"create_version_by,omitempty"`
	CreateVersionSrc string    `json:"create_version_source,omitempty"`
	DeleteSerial     *int64    `json:"delete_serial,omitempty"`
	DeleteVersionID  string    `json:"delete_version_id,omitempty"`
	DeleteVersionAt  time.Time `json:"delete_version_at,omitempty"`
	DeleteVersionBy  string    `json:"delete_version_by,omitempty"`
	DeleteVersionSrc string    `json:"delete_version_source,omitempty"`
}

// ResourceRollbackPreview is the operator-facing dry-run summary for one exact
// resource address replay into current state bookkeeping.
type ResourceRollbackPreview struct {
	StateName      string           `json:"state"`
	Address        string           `json:"address"`
	Action         string           `json:"action"`
	CurrentVersion StateVersionInfo `json:"current"`
	TargetVersion  StateVersionInfo `json:"target"`
	CurrentExists  bool             `json:"current_exists"`
	TargetExists   bool             `json:"target_exists"`
	CurrentAttrs   json.RawMessage  `json:"current_attrs,omitempty"`
	TargetAttrs    json.RawMessage  `json:"target_attrs,omitempty"`
	CurrentSens    json.RawMessage  `json:"current_sensitive_paths,omitempty"`
	TargetSens     json.RawMessage  `json:"target_sensitive_paths,omitempty"`
	Dependencies   []string         `json:"dependencies,omitempty"`
	Dependents     []string         `json:"dependents,omitempty"`
	Warnings       []string         `json:"warnings,omitempty"`
}

type resourceVersionSnapshot struct {
	Address         string
	Mode            string
	Type            string
	Name            string
	Provider        string
	ModulePath      string
	IndexKind       string
	IndexValue      string
	SchemaVersion   int
	Attributes      json.RawMessage
	SensitivePaths  json.RawMessage
	DependenciesRaw json.RawMessage
}

// ListCurrentResources returns live resource instances from the current state.
// addressGlob uses the same lightweight matching semantics as reservations and
// CLI filters: empty means "all".
func (s *Store) ListCurrentResources(ctx context.Context, stateName, addressGlob string, limit int) ([]ResourceSnapshot, error) {
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
		       r.attributes,
		       r.sensitive_paths
		FROM   states s
		JOIN   resources r
		       ON r.state_id = s.id
		      AND r.delete_serial IS NULL
		WHERE  ` + where + `
		ORDER  BY r.address`
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT $%d", len(args)+1)
		args = append(args, limit)
	}
	rows, err := s.pool.Query(ctx, q, args...)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrStateNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("list current resources: %w", err)
	}
	defer rows.Close()

	out := make([]ResourceSnapshot, 0)
	for rows.Next() {
		var row ResourceSnapshot
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
		); err != nil {
			return nil, fmt.Errorf("scan current resource: %w", err)
		}
		if matchAddressPattern(strings.TrimSpace(addressGlob), row.Address) {
			out = append(out, row)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate current resources: %w", err)
	}
	if len(out) == 0 {
		if _, _, err := s.lookupStateIDAndCurrent(ctx, stateName); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// GetCurrentResource returns one exact live resource instance from the current state.
func (s *Store) GetCurrentResource(ctx context.Context, stateName, address string) (*ResourceSnapshot, error) {
	rows, err := s.ListCurrentResources(ctx, stateName, address, 0)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if row.Address == address {
			copied := row
			return &copied, nil
		}
	}
	return nil, ErrStateNotFound
}

// ListResourceHistory returns lifecycle spans for one exact resource address.
func (s *Store) ListResourceHistory(ctx context.Context, stateName, address string, limit int) ([]ResourceHistoryEntry, error) {
	if strings.TrimSpace(address) == "" {
		return nil, fmt.Errorf("resource address is required")
	}
	where, args := s.stateByNameWhere(ctx, stateName)
	args = append(args, strings.TrimSpace(address))
	addressArg := len(args)
	q := `
		SELECT r.address,
		       r.mode,
		       r.type,
		       r.provider,
		       r.create_serial,
		       sv_create.id,
		       sv_create.created_at,
		       COALESCE(sv_create.created_by, ''),
		       COALESCE(sv_create.source, ''),
		       r.delete_serial,
		       COALESCE(sv_delete.id::text, ''),
		       COALESCE(sv_delete.created_at, timestamptz '0001-01-01'),
		       COALESCE(sv_delete.created_by, ''),
		       COALESCE(sv_delete.source, '')
		FROM   states s
		JOIN   resources r
		       ON r.state_id = s.id
		      AND r.address = $` + fmt.Sprint(addressArg) + `
		JOIN   state_versions sv_create
		       ON sv_create.state_id = s.id
		      AND sv_create.serial = r.create_serial
		LEFT   JOIN state_versions sv_delete
		       ON sv_delete.state_id = s.id
		      AND sv_delete.serial = r.delete_serial
		WHERE  ` + where + `
		ORDER  BY r.create_serial DESC`
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT $%d", len(args)+1)
		args = append(args, limit)
	}
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list resource history: %w", err)
	}
	defer rows.Close()

	var out []ResourceHistoryEntry
	for rows.Next() {
		var row ResourceHistoryEntry
		var deleteVersionID, deleteBy, deleteSrc string
		var deleteAt time.Time
		if err := rows.Scan(
			&row.Address,
			&row.Mode,
			&row.Type,
			&row.Provider,
			&row.CreateSerial,
			&row.CreateVersionID,
			&row.CreateVersionAt,
			&row.CreateVersionBy,
			&row.CreateVersionSrc,
			&row.DeleteSerial,
			&deleteVersionID,
			&deleteAt,
			&deleteBy,
			&deleteSrc,
		); err != nil {
			return nil, fmt.Errorf("scan resource history: %w", err)
		}
		if row.DeleteSerial != nil {
			row.DeleteVersionID = deleteVersionID
			row.DeleteVersionAt = deleteAt
			row.DeleteVersionBy = deleteBy
			row.DeleteVersionSrc = deleteSrc
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate resource history: %w", err)
	}
	if len(out) == 0 {
		if _, _, err := s.lookupStateIDAndCurrent(ctx, stateName); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// PreviewReplayResourceVersion explains what replaying one exact address from
// a historical version into the current state would do.
func (s *Store) PreviewReplayResourceVersion(ctx context.Context, stateName, address, ref string) (*ResourceRollbackPreview, error) {
	stateID, currentID, err := s.lookupStateIDAndCurrent(ctx, stateName)
	if err != nil {
		return nil, err
	}
	currentInfo, err := s.resolveVersionInfo(ctx, stateID, currentID, "current")
	if err != nil {
		return nil, err
	}
	targetInfo, err := s.resolveVersionInfo(ctx, stateID, currentID, ref)
	if err != nil {
		return nil, err
	}
	currentSnap, err := s.getResourceAtVersion(ctx, stateID, currentInfo.Serial, address)
	if err != nil {
		return nil, err
	}
	targetSnap, err := s.getResourceAtVersion(ctx, stateID, targetInfo.Serial, address)
	if err != nil {
		return nil, err
	}
	dependents := currentDependents(ctx, s, stateID, currentInfo.Serial, address)

	return &ResourceRollbackPreview{
		StateName:      stateName,
		Address:        address,
		Action:         classifyResourceReplaySnapshot(currentSnap, targetSnap),
		CurrentVersion: *currentInfo,
		TargetVersion:  *targetInfo,
		CurrentExists:  currentSnap != nil,
		TargetExists:   targetSnap != nil,
		CurrentAttrs:   snapshotAttrs(currentSnap),
		TargetAttrs:    snapshotAttrs(targetSnap),
		CurrentSens:    snapshotSensitive(currentSnap),
		TargetSens:     snapshotSensitive(targetSnap),
		Dependencies:   previewDependenciesSnapshot(targetSnap, currentSnap),
		Dependents:     dependents,
		Warnings:       previewWarningsSnapshot(currentSnap, targetSnap, dependents),
	}, nil
}

// ReplayResourceVersion writes a new current state version by patching one
// exact Terraform address from a historical version into the current state.
func (s *Store) ReplayResourceVersion(ctx context.Context, stateName, address, ref, actor string) (*StateVersionInfo, *ResourceRollbackPreview, error) {
	preview, err := s.PreviewReplayResourceVersion(ctx, stateName, address, ref)
	if err != nil {
		return nil, nil, err
	}
	if preview.Action == "no-op" {
		return nil, preview, nil
	}

	currentInfo, currentRaw, targetInfo, targetRaw, err := s.loadResourceReplayInputs(ctx, stateName, ref)
	if err != nil {
		return nil, nil, err
	}
	currentState, err := tfstate.Parse(currentRaw)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrInvalidState, err)
	}
	targetState, err := tfstate.Parse(targetRaw)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrInvalidState, err)
	}
	currentLoc, _ := findResourceInstance(currentState, address)
	targetLoc, _ := findResourceInstance(targetState, address)

	patched, err := patchStateResource(currentState, currentLoc, targetLoc)
	if err != nil {
		return nil, nil, err
	}
	source := "resource-rollback:" + strings.TrimSpace(address)
	stateID, newSerial, err := s.replayResourceVersionDelta(ctx, stateName, strings.TrimSpace(address), preview.TargetVersion.Serial, patched, actor, source)
	if err != nil {
		return nil, nil, err
	}
	newInfo, err := s.resolveVersionInfo(ctx, stateID, "", fmt.Sprintf("%d", newSerial))
	if err != nil {
		return nil, nil, err
	}
	preview.CurrentVersion = *currentInfo
	preview.TargetVersion = *targetInfo
	return newInfo, preview, nil
}

func (s *Store) replayResourceVersionDelta(ctx context.Context, stateName, address string, targetSerial int64, patched *tfstate.State, actor, source string) (string, int64, error) {
	var (
		stateID   string
		newSerial int64
	)
	tenantID := auth.TenantFromContext(ctx)
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := enforceTenantLifecycleActive(ctx, tx, tenantID); err != nil {
			return err
		}
		currentStateID, currentVersionID, err := s.lookupStateIDAndCurrentTx(ctx, tx, stateName)
		if err != nil {
			return err
		}
		stateID = currentStateID
		if strings.TrimSpace(currentVersionID) == "" {
			return ErrStateNotFound
		}
		currentOpen, err := loadOpenResourceRow(ctx, tx, currentStateID, address)
		if err != nil && !errors.Is(err, ErrStateNotFound) {
			return err
		}
		targetRow, err := loadResourceRowAtSerial(ctx, tx, currentStateID, address, targetSerial)
		if err != nil {
			return err
		}
		newSerial, err = computeNextStateSerial(ctx, tx, currentStateID)
		if err != nil {
			return err
		}
		patched.Serial = newSerial
		raw, err := json.Marshal(patched)
		if err != nil {
			return fmt.Errorf("marshal patched state: %w", err)
		}
		versionID, err := insertDerivedStateVersion(ctx, tx, tenantID, currentStateID, newSerial, patched.TerraformVersion, raw, source, actor)
		if err != nil {
			return err
		}
		if currentOpen != nil {
			if err := closeResources(ctx, tx, []string{currentOpen.id}, newSerial); err != nil {
				return fmt.Errorf("close current replay row: %w", err)
			}
		}
		if targetRow != nil {
			if err := insertResourceRows(ctx, tx, tenantID, currentStateID, newSerial, []resourceRow{targetRow.row}); err != nil {
				return err
			}
		}
		if err := insertOutputs(ctx, tx, tenantID, versionID, patched); err != nil {
			return err
		}
		if err := finalizeDerivedStateVersion(ctx, tx, tenantID, currentStateID, versionID, actor, source, newSerial); err != nil {
			return err
		}
		return nil
	})
	return stateID, newSerial, err
}

func (s *Store) loadResourceReplayInputs(ctx context.Context, stateName, ref string) (*StateVersionInfo, []byte, *StateVersionInfo, []byte, error) {
	currentInfo, currentRaw, err := s.GetVersionRaw(ctx, stateName, "current")
	if err != nil {
		return nil, nil, nil, nil, err
	}
	targetInfo, targetRaw, err := s.GetVersionRaw(ctx, stateName, ref)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	return currentInfo, currentRaw, targetInfo, targetRaw, nil
}

type resourceLocation struct {
	ResourceIndex int
	InstanceIndex int
	Resource      tfstate.Resource
	Instance      tfstate.ResourceInstance
}

func findResourceInstance(st *tfstate.State, address string) (*resourceLocation, error) {
	if st == nil {
		return nil, nil
	}
	for resourceIndex, resource := range st.Resources {
		for instanceIndex, instance := range resource.Instances {
			addr, err := tfstate.InstanceAddress(resource, instance)
			if err != nil {
				return nil, err
			}
			if addr == address {
				return &resourceLocation{
					ResourceIndex: resourceIndex,
					InstanceIndex: instanceIndex,
					Resource:      resource,
					Instance:      instance,
				}, nil
			}
		}
	}
	return nil, nil
}

func classifyResourceReplay(currentExists, targetExists bool, currentLoc, targetLoc *resourceLocation) string {
	switch {
	case currentExists && targetExists:
		if resourceLocationsEqual(currentLoc, targetLoc) {
			return "no-op"
		}
		return "replace"
	case !currentExists && targetExists:
		return "restore"
	case currentExists && !targetExists:
		return "remove"
	default:
		return "no-op"
	}
}

func resourceLocationsEqual(a, b *resourceLocation) bool {
	if a == nil || b == nil {
		return a == b
	}
	left, _ := json.Marshal(struct {
		Resource tfstate.Resource         `json:"resource"`
		Instance tfstate.ResourceInstance `json:"instance"`
	}{Resource: a.Resource, Instance: a.Instance})
	right, _ := json.Marshal(struct {
		Resource tfstate.Resource         `json:"resource"`
		Instance tfstate.ResourceInstance `json:"instance"`
	}{Resource: b.Resource, Instance: b.Instance})
	return string(left) == string(right)
}

func patchStateResource(current *tfstate.State, currentLoc, targetLoc *resourceLocation) (*tfstate.State, error) {
	if current == nil {
		return nil, fmt.Errorf("current state is required")
	}
	clone := *current
	clone.Resources = slices.Clone(current.Resources)

	if currentLoc != nil {
		resource := clone.Resources[currentLoc.ResourceIndex]
		resource.Instances = slices.Clone(resource.Instances)
		resource.Instances = append(resource.Instances[:currentLoc.InstanceIndex], resource.Instances[currentLoc.InstanceIndex+1:]...)
		if len(resource.Instances) == 0 {
			clone.Resources = append(clone.Resources[:currentLoc.ResourceIndex], clone.Resources[currentLoc.ResourceIndex+1:]...)
		} else {
			clone.Resources[currentLoc.ResourceIndex] = resource
		}
	}

	if targetLoc == nil {
		return &clone, nil
	}
	inserted := false
	for i, resource := range clone.Resources {
		if resource.Mode == targetLoc.Resource.Mode &&
			resource.Type == targetLoc.Resource.Type &&
			resource.Name == targetLoc.Resource.Name &&
			resource.Provider == targetLoc.Resource.Provider &&
			resource.Module == targetLoc.Resource.Module {
			resource.Instances = append(slices.Clone(resource.Instances), targetLoc.Instance)
			clone.Resources[i] = resource
			inserted = true
			break
		}
	}
	if !inserted {
		resource := targetLoc.Resource
		resource.Instances = []tfstate.ResourceInstance{targetLoc.Instance}
		clone.Resources = append(clone.Resources, resource)
	}
	return &clone, nil
}

func matchAddressPattern(pattern, address string) bool {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return true
	}
	ok, err := path.Match(pattern, address)
	if err != nil {
		return false
	}
	return ok
}

func (s *Store) getResourceAtVersion(ctx context.Context, stateID string, serial int64, address string) (*resourceVersionSnapshot, error) {
	if strings.TrimSpace(stateID) == "" || strings.TrimSpace(address) == "" || serial <= 0 {
		return nil, nil
	}
	const q = `
		SELECT address,
		       mode,
		       type,
		       name,
		       provider,
		       module_path,
		       index_kind,
		       COALESCE(index_value, ''),
		       schema_version,
		       attributes,
		       sensitive_paths,
		       dependencies_raw
		FROM   resources
		WHERE  state_id = $1
		  AND  address = $2
		  AND  create_serial <= $3
		  AND  (delete_serial IS NULL OR delete_serial > $3)
		LIMIT  1`
	var row resourceVersionSnapshot
	err := s.pool.QueryRow(ctx, q, stateID, strings.TrimSpace(address), serial).Scan(
		&row.Address,
		&row.Mode,
		&row.Type,
		&row.Name,
		&row.Provider,
		&row.ModulePath,
		&row.IndexKind,
		&row.IndexValue,
		&row.SchemaVersion,
		&row.Attributes,
		&row.SensitivePaths,
		&row.DependenciesRaw,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load resource at version: %w", err)
	}
	return &row, nil
}

func loadResourceRowAtSerial(ctx context.Context, tx pgx.Tx, stateID, address string, serial int64) (*openResourceRow, error) {
	if strings.TrimSpace(stateID) == "" || strings.TrimSpace(address) == "" || serial <= 0 {
		return nil, nil
	}
	const q = `
		SELECT r.id, r.address, r.mode, r.type, r.name, r.provider, r.module_path,
		       r.index_kind, r.index_value, r.schema_version, r.attributes::text, r.sensitive_paths::text,
		       r.dependencies_raw::text, r.attributes_hash
		FROM   resources r
		WHERE  r.state_id = $1
		  AND  r.address = $2
		  AND  r.create_serial <= $3
		  AND  (r.delete_serial IS NULL OR r.delete_serial > $3)
		ORDER  BY r.create_serial DESC
		LIMIT  1`
	var (
		item     openResourceRow
		indexVal *string
	)
	err := tx.QueryRow(ctx, q, stateID, address, serial).Scan(
		&item.id,
		&item.row.address,
		&item.row.mode,
		&item.row.rtype,
		&item.row.name,
		&item.row.provider,
		&item.row.modulePath,
		&item.row.indexKind,
		&indexVal,
		&item.row.schemaVersion,
		&item.row.attributes,
		&item.row.sensitivePaths,
		&item.row.dependenciesRaw,
		&item.row.attributesHash,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load resource row at serial: %w", err)
	}
	if indexVal != nil {
		item.row.indexValue = *indexVal
	}
	return &item, nil
}

func currentDependents(ctx context.Context, s *Store, stateID string, serial int64, address string) []string {
	if s == nil || strings.TrimSpace(stateID) == "" || serial <= 0 || strings.TrimSpace(address) == "" {
		return nil
	}
	const q = `
		SELECT DISTINCT r_from.address
		FROM   resources r_from
		CROSS  JOIN LATERAL jsonb_array_elements_text(r_from.dependencies_raw) AS dep(addr)
		WHERE  r_from.state_id = $1
		  AND  r_from.create_serial <= $2
		  AND  (r_from.delete_serial IS NULL OR r_from.delete_serial > $2)
		  AND  (dep.addr = $3 OR starts_with($3, dep.addr || '[') OR starts_with(dep.addr, $3 || '['))
		ORDER  BY r_from.address`
	rows, err := s.pool.Query(ctx, q, stateID, serial, address)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var dep string
		if err := rows.Scan(&dep); err != nil {
			return nil
		}
		out = append(out, dep)
	}
	if err := rows.Err(); err != nil {
		return nil
	}
	return out
}

func previewWarningsSnapshot(currentSnap, targetSnap *resourceVersionSnapshot, dependents []string) []string {
	var out []string
	switch {
	case currentSnap == nil && targetSnap == nil:
		out = append(out, "address does not exist in current or target version")
	case currentSnap != nil && targetSnap == nil:
		out = append(out, "this will remove the resource from current state bookkeeping")
	case currentSnap == nil && targetSnap != nil:
		out = append(out, "this will restore the resource into current state bookkeeping")
	default:
		out = append(out, "this will replace the current resource instance with historical state")
	}
	if len(dependents) > 0 {
		out = append(out, fmt.Sprintf("%d live resource(s) currently depend on this address", len(dependents)))
	}
	if deps := snapshotDependencies(targetSnap); len(deps) > 0 {
		out = append(out, fmt.Sprintf("historical resource instance depends on %d address(es)", len(deps)))
	}
	return out
}

func classifyResourceReplaySnapshot(currentSnap, targetSnap *resourceVersionSnapshot) string {
	switch {
	case currentSnap != nil && targetSnap != nil:
		if resourceSnapshotsEqual(currentSnap, targetSnap) {
			return "no-op"
		}
		return "replace"
	case currentSnap == nil && targetSnap != nil:
		return "restore"
	case currentSnap != nil && targetSnap == nil:
		return "remove"
	default:
		return "no-op"
	}
}

func resourceSnapshotsEqual(a, b *resourceVersionSnapshot) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Address == b.Address &&
		a.Mode == b.Mode &&
		a.Type == b.Type &&
		a.Name == b.Name &&
		a.Provider == b.Provider &&
		a.ModulePath == b.ModulePath &&
		a.IndexKind == b.IndexKind &&
		a.IndexValue == b.IndexValue &&
		a.SchemaVersion == b.SchemaVersion &&
		string(a.Attributes) == string(b.Attributes) &&
		string(a.SensitivePaths) == string(b.SensitivePaths) &&
		string(a.DependenciesRaw) == string(b.DependenciesRaw)
}

func previewDependenciesSnapshot(targetSnap, currentSnap *resourceVersionSnapshot) []string {
	snap := targetSnap
	if snap == nil {
		snap = currentSnap
	}
	deps := snapshotDependencies(snap)
	sortStrings(deps)
	return deps
}

func snapshotDependencies(snap *resourceVersionSnapshot) []string {
	if snap == nil || len(snap.DependenciesRaw) == 0 {
		return nil
	}
	var deps []string
	if err := json.Unmarshal(snap.DependenciesRaw, &deps); err != nil {
		return nil
	}
	return deps
}

func snapshotAttrs(snap *resourceVersionSnapshot) json.RawMessage {
	if snap == nil || len(snap.Attributes) == 0 {
		return nil
	}
	return append(json.RawMessage(nil), snap.Attributes...)
}

func snapshotSensitive(snap *resourceVersionSnapshot) json.RawMessage {
	if snap == nil || len(snap.SensitivePaths) == 0 {
		return nil
	}
	return append(json.RawMessage(nil), snap.SensitivePaths...)
}
