package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/kilolockio/kilolock/internal/tfstate"
	"github.com/kilolockio/kilolock/pkg/auth"
)

// ResourceMutationPreview is the operator-facing dry-run summary for a native
// state-engine exact-address mutation against current state.
type ResourceMutationPreview struct {
	StateName      string           `json:"state"`
	Action         string           `json:"action"`
	Address        string           `json:"address"`
	ToAddress      string           `json:"to_address,omitempty"`
	CurrentVersion StateVersionInfo `json:"current"`
	CurrentExists  bool             `json:"current_exists"`
	TargetExists   bool             `json:"target_exists"`
	CurrentAttrs   json.RawMessage  `json:"current_attrs,omitempty"`
	Dependencies   []string         `json:"dependencies,omitempty"`
	Dependents     []string         `json:"dependents,omitempty"`
	Warnings       []string         `json:"warnings,omitempty"`
}

func (loc *resourceLocation) Address() string {
	if loc == nil {
		return ""
	}
	address, _ := tfstate.InstanceAddress(loc.Resource, loc.Instance)
	return address
}

func (s *Store) PreviewRemoveResourceCurrent(ctx context.Context, stateName, address string) (*ResourceMutationPreview, error) {
	address = strings.TrimSpace(address)
	if address == "" {
		return nil, fmt.Errorf("resource address is required")
	}
	currentInfo, currentRaw, err := s.GetVersionRaw(ctx, stateName, "current")
	if err != nil {
		return nil, err
	}
	currentState, err := tfstate.Parse(currentRaw)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidState, err)
	}
	currentLoc, err := findResourceInstance(currentState, address)
	if err != nil {
		return nil, err
	}
	preview := &ResourceMutationPreview{
		StateName:      stateName,
		Action:         "no-op",
		Address:        address,
		CurrentVersion: *currentInfo,
	}
	if currentLoc == nil {
		return preview, nil
	}
	preview.Action = "remove"
	preview.CurrentExists = true
	preview.CurrentAttrs = slices.Clone(currentLoc.Instance.Attributes)
	preview.Dependencies = slices.Clone(currentLoc.Instance.Dependencies)
	preview.Dependents = collectExactDependents(currentState, address)
	if len(preview.Dependents) > 0 {
		preview.Warnings = append(preview.Warnings, "current state contains dependents that still reference this address")
	}
	return preview, nil
}

func (s *Store) ApplyRemoveResourceCurrent(ctx context.Context, stateName, address, actor string) (*StateVersionInfo, *ResourceMutationPreview, error) {
	preview, err := s.PreviewRemoveResourceCurrent(ctx, stateName, address)
	if err != nil {
		return nil, nil, err
	}
	if preview.Action == "no-op" {
		return nil, preview, nil
	}
	stateID, newSerial, err := s.applyRemoveResourceCurrentDelta(ctx, stateName, strings.TrimSpace(address), actor)
	if err != nil {
		return nil, nil, err
	}
	newInfo, err := s.resolveVersionInfo(ctx, stateID, "", fmt.Sprintf("%d", newSerial))
	if err != nil {
		return nil, nil, err
	}
	return newInfo, preview, nil
}

func (s *Store) PreviewMoveResourceCurrent(ctx context.Context, stateName, from, to string) (*ResourceMutationPreview, error) {
	from = strings.TrimSpace(from)
	to = strings.TrimSpace(to)
	if from == "" || to == "" {
		return nil, fmt.Errorf("source and destination addresses are required")
	}
	currentInfo, currentRaw, err := s.GetVersionRaw(ctx, stateName, "current")
	if err != nil {
		return nil, err
	}
	currentState, err := tfstate.Parse(currentRaw)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidState, err)
	}
	fromLoc, err := findResourceInstance(currentState, from)
	if err != nil {
		return nil, err
	}
	toLoc, err := findResourceInstance(currentState, to)
	if err != nil {
		return nil, err
	}
	preview := &ResourceMutationPreview{
		StateName:      stateName,
		Action:         "no-op",
		Address:        from,
		ToAddress:      to,
		CurrentVersion: *currentInfo,
		CurrentExists:  fromLoc != nil,
		TargetExists:   toLoc != nil,
	}
	if fromLoc == nil {
		return preview, nil
	}
	preview.CurrentAttrs = slices.Clone(fromLoc.Instance.Attributes)
	preview.Dependencies = slices.Clone(fromLoc.Instance.Dependencies)
	preview.Dependents = collectExactDependents(currentState, from)
	if toLoc != nil {
		preview.Action = "conflict"
		preview.Warnings = append(preview.Warnings, "destination address already exists in current state")
		return preview, nil
	}
	preview.Action = "move"
	return preview, nil
}

func (s *Store) ApplyMoveResourceCurrent(ctx context.Context, stateName, from, to, actor string) (*StateVersionInfo, *ResourceMutationPreview, error) {
	preview, err := s.PreviewMoveResourceCurrent(ctx, stateName, from, to)
	if err != nil {
		return nil, nil, err
	}
	switch preview.Action {
	case "no-op":
		return nil, preview, nil
	case "conflict":
		return nil, preview, fmt.Errorf("destination address already exists in current state")
	}
	stateID, newSerial, err := s.applyMoveResourceCurrentDelta(ctx, stateName, strings.TrimSpace(from), strings.TrimSpace(to), actor)
	if err != nil {
		return nil, nil, err
	}
	newInfo, err := s.resolveVersionInfo(ctx, stateID, "", fmt.Sprintf("%d", newSerial))
	if err != nil {
		return nil, nil, err
	}
	return newInfo, preview, nil
}

func (s *Store) applyRemoveResourceCurrentDelta(ctx context.Context, stateName, address, actor string) (string, int64, error) {
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
		_, currentRaw, err := s.resolveVersionTx(ctx, tx, currentStateID, currentVersionID, "current")
		if err != nil {
			return err
		}
		currentState, err := tfstate.Parse(currentRaw)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidState, err)
		}
		currentLoc, err := findResourceInstance(currentState, address)
		if err != nil {
			return err
		}
		if currentLoc == nil {
			return nil
		}
		patched, err := patchStateResource(currentState, currentLoc, nil)
		if err != nil {
			return err
		}
		row, err := loadOpenResourceRow(ctx, tx, currentStateID, address)
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
		versionID, err := insertDerivedStateVersion(ctx, tx, tenantID, currentStateID, newSerial, patched.TerraformVersion, raw, "resource-remove:"+address, actor)
		if err != nil {
			return err
		}
		if err := closeResources(ctx, tx, []string{row.id}, newSerial); err != nil {
			return fmt.Errorf("close removed resource: %w", err)
		}
		if err := insertOutputs(ctx, tx, tenantID, versionID, patched); err != nil {
			return err
		}
		if err := finalizeDerivedStateVersion(ctx, tx, tenantID, currentStateID, versionID, actor, "resource-remove:"+address, newSerial); err != nil {
			return err
		}
		return nil
	})
	return stateID, newSerial, err
}

func (s *Store) applyMoveResourceCurrentDelta(ctx context.Context, stateName, from, to, actor string) (string, int64, error) {
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
		_, currentRaw, err := s.resolveVersionTx(ctx, tx, currentStateID, currentVersionID, "current")
		if err != nil {
			return err
		}
		currentState, err := tfstate.Parse(currentRaw)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidState, err)
		}
		fromLoc, err := findResourceInstance(currentState, from)
		if err != nil {
			return err
		}
		if fromLoc == nil {
			return nil
		}
		patched, err := patchMoveStateResource(currentState, fromLoc, to)
		if err != nil {
			return err
		}
		fromRow, err := loadOpenResourceRow(ctx, tx, currentStateID, from)
		if err != nil {
			return err
		}
		if existingTo, err := loadOpenResourceRow(ctx, tx, currentStateID, to); err != nil && !errors.Is(err, ErrStateNotFound) {
			return err
		} else if existingTo != nil {
			return fmt.Errorf("destination address already exists in current state")
		}
		dependents, err := loadOpenDependentResourceRows(ctx, tx, currentStateID, from)
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
		source := "resource-move:" + from + "->" + to
		versionID, err := insertDerivedStateVersion(ctx, tx, tenantID, currentStateID, newSerial, patched.TerraformVersion, raw, source, actor)
		if err != nil {
			return err
		}
		closeIDs := []string{fromRow.id}
		reopened := make([]resourceRow, 0, 1+len(dependents))
		movedRow, err := movedResourceRow(fromRow.row, to, from)
		if err != nil {
			return err
		}
		reopened = append(reopened, movedRow)
		for _, dep := range dependents {
			closeIDs = append(closeIDs, dep.id)
			reopened = append(reopened, rewriteResourceRowDependencies(dep.row, from, to))
		}
		if err := closeResources(ctx, tx, closeIDs, newSerial); err != nil {
			return fmt.Errorf("close moved resource rows: %w", err)
		}
		if err := insertResourceRows(ctx, tx, tenantID, currentStateID, newSerial, reopened); err != nil {
			return err
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

type openResourceRow struct {
	id  string
	row resourceRow
}

func loadOpenResourceRow(ctx context.Context, tx pgx.Tx, stateID, address string) (*openResourceRow, error) {
	rows, err := loadOpenResourceRowsByQuery(ctx, tx, stateID, `r.address = $2`, address)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, ErrStateNotFound
	}
	return &rows[0], nil
}

func loadOpenDependentResourceRows(ctx context.Context, tx pgx.Tx, stateID, address string) ([]openResourceRow, error) {
	filter := marshalDependencies([]string{address})
	return loadOpenResourceRowsByQuery(ctx, tx, stateID, `r.address <> $2 AND r.dependencies_raw @> $3::jsonb`, address, filter)
}

func loadOpenResourceRowsByQuery(ctx context.Context, tx pgx.Tx, stateID, predicate string, args ...any) ([]openResourceRow, error) {
	q := `
		SELECT r.id, r.address, r.mode, r.type, r.name, r.provider, r.module_path,
		       r.index_kind, r.index_value, r.schema_version, r.attributes::text, r.sensitive_paths::text,
		       r.dependencies_raw::text, r.attributes_hash
		FROM   resources r
		WHERE  r.state_id = $1
		  AND  r.delete_serial IS NULL
		  AND  ` + predicate + `
		ORDER  BY r.address`
	queryArgs := append([]any{stateID}, args...)
	rows, err := tx.Query(ctx, q, queryArgs...)
	if err != nil {
		return nil, fmt.Errorf("load open resource rows: %w", err)
	}
	defer rows.Close()
	out := make([]openResourceRow, 0)
	for rows.Next() {
		var (
			item     openResourceRow
			indexVal *string
		)
		if err := rows.Scan(
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
		); err != nil {
			return nil, fmt.Errorf("scan open resource row: %w", err)
		}
		if indexVal != nil {
			item.row.indexValue = *indexVal
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate open resource rows: %w", err)
	}
	return out, nil
}

func movedResourceRow(from resourceRow, toAddress, oldAddress string) (resourceRow, error) {
	toResource, toInstance, err := tfstate.ParseInstanceAddress(strings.TrimSpace(toAddress))
	if err != nil {
		return resourceRow{}, err
	}
	indexKind, indexValue, err := toInstance.DecodeIndex()
	if err != nil {
		return resourceRow{}, err
	}
	row := rewriteResourceRowDependencies(from, oldAddress, strings.TrimSpace(toAddress))
	row.address = strings.TrimSpace(toAddress)
	row.mode = toResource.Mode
	row.rtype = toResource.Type
	row.name = toResource.Name
	row.modulePath = toResource.Module
	row.indexKind = indexKind.String()
	row.indexValue = nil
	if indexKind != tfstate.IndexNone {
		row.indexValue = indexValue
	}
	row.attributesHash = hashResourceContent(row)
	return row, nil
}

func rewriteResourceRowDependencies(row resourceRow, from, to string) resourceRow {
	if strings.TrimSpace(from) == "" || strings.TrimSpace(to) == "" || from == to {
		return row
	}
	deps := decodeDependencies(row.dependenciesRaw)
	if len(deps) == 0 {
		return row
	}
	changed := false
	for i, dep := range deps {
		if dep == from {
			deps[i] = to
			changed = true
		}
	}
	if !changed {
		return row
	}
	slices.Sort(deps)
	row.dependenciesRaw = marshalDependencies(deps)
	row.attributesHash = hashResourceContent(row)
	return row
}

func decodeDependencies(raw string) []string {
	if strings.TrimSpace(raw) == "" || strings.TrimSpace(raw) == "[]" {
		return nil
	}
	var deps []string
	if err := json.Unmarshal([]byte(raw), &deps); err != nil {
		return nil
	}
	return deps
}

func computeNextStateSerial(ctx context.Context, tx pgx.Tx, stateID string) (int64, error) {
	var nextSerial int64
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(MAX(serial), 0) + 1 FROM state_versions WHERE state_id = $1`,
		stateID,
	).Scan(&nextSerial); err != nil {
		return 0, fmt.Errorf("compute next serial: %w", err)
	}
	return nextSerial, nil
}

func insertDerivedStateVersion(ctx context.Context, tx pgx.Tx, tenantID, stateID string, serial int64, terraformVersion string, raw []byte, source, actor string) (string, error) {
	var versionID string
	err := tx.QueryRow(ctx,
		`INSERT INTO state_versions
		 	(state_id, tenant_id, serial, terraform_version, raw_state, source, created_by)
		 VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7)
		 RETURNING id`,
		stateID, tenantID, serial, terraformVersion, string(raw), source, actor,
	).Scan(&versionID)
	if err != nil {
		if isUniqueViolation(err, "state_versions_state_id_serial_key") {
			return "", ErrSerialConflict
		}
		return "", fmt.Errorf("insert state_version: %w", err)
	}
	return versionID, nil
}

func finalizeDerivedStateVersion(ctx context.Context, tx pgx.Tx, tenantID, stateID, versionID, actor, source string, serial int64) error {
	if _, err := tx.Exec(ctx,
		`UPDATE states SET current_version_id = $1, updated_at = now() WHERE id = $2`,
		versionID, stateID,
	); err != nil {
		return fmt.Errorf("update current_version_id: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO events (kind, tenant_id, state_id, state_version_id, actor, payload)
		 VALUES ('state_write', $1, $2, $3, $4, jsonb_build_object('source', $5::text, 'serial', $6::bigint))`,
		tenantID, stateID, versionID, actor, source, serial,
	); err != nil {
		return err
	}
	return nil
}

func patchMoveStateResource(current *tfstate.State, fromLoc *resourceLocation, toAddress string) (*tfstate.State, error) {
	if current == nil {
		return nil, fmt.Errorf("current state is required")
	}
	if fromLoc == nil {
		return nil, fmt.Errorf("source address not found in current state")
	}
	toResource, toInstance, err := tfstate.ParseInstanceAddress(strings.TrimSpace(toAddress))
	if err != nil {
		return nil, err
	}
	if existing, err := findResourceInstance(current, strings.TrimSpace(toAddress)); err != nil {
		return nil, err
	} else if existing != nil {
		return nil, fmt.Errorf("destination address already exists in current state")
	}

	clone, err := patchStateResource(current, fromLoc, nil)
	if err != nil {
		return nil, err
	}
	moved := fromLoc.Instance
	moved.IndexKey = slices.Clone(toInstance.IndexKey)
	inserted := false
	for i, resource := range clone.Resources {
		if resource.Mode == toResource.Mode &&
			resource.Type == toResource.Type &&
			resource.Name == toResource.Name &&
			resource.Provider == fromLoc.Resource.Provider &&
			resource.Module == toResource.Module {
			resource.Instances = append(slices.Clone(resource.Instances), moved)
			clone.Resources[i] = resource
			inserted = true
			break
		}
	}
	if !inserted {
		resource := tfstate.Resource{
			Module:    toResource.Module,
			Mode:      toResource.Mode,
			Type:      toResource.Type,
			Name:      toResource.Name,
			Provider:  fromLoc.Resource.Provider,
			Instances: []tfstate.ResourceInstance{moved},
		}
		clone.Resources = append(clone.Resources, resource)
	}
	rewriteDependencies(clone, strings.TrimSpace(fromLoc.Address()), strings.TrimSpace(toAddress))
	return clone, nil
}

func rewriteDependencies(state *tfstate.State, from, to string) {
	if state == nil || from == "" || to == "" || from == to {
		return
	}
	for i, resource := range state.Resources {
		resource.Instances = slices.Clone(resource.Instances)
		for j, instance := range resource.Instances {
			if len(instance.Dependencies) == 0 {
				continue
			}
			deps := slices.Clone(instance.Dependencies)
			for k, dep := range deps {
				if dep == from {
					deps[k] = to
				}
			}
			instance.Dependencies = deps
			resource.Instances[j] = instance
		}
		state.Resources[i] = resource
	}
}

func collectExactDependents(state *tfstate.State, address string) []string {
	if state == nil || strings.TrimSpace(address) == "" {
		return nil
	}
	out := make([]string, 0)
	for _, resource := range state.Resources {
		for _, instance := range resource.Instances {
			found := false
			for _, dep := range instance.Dependencies {
				if dep == address {
					found = true
					break
				}
			}
			if !found {
				continue
			}
			addr, err := tfstate.InstanceAddress(resource, instance)
			if err != nil || addr == "" {
				continue
			}
			out = append(out, addr)
		}
	}
	slices.Sort(out)
	return out
}
