package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/kilolockio/kilolock/internal/tfstate"
)

// normalize projects a parsed Terraform state into the resources and
// outputs tables for the given state_version, using lifecycle ranges
// (see ADR 0004 / docs/schema.md).
//
// The contract:
//
//   - Each resource is reduced to a (address, content) pair plus a
//     stable attributes_hash over its normalize-relevant content.
//
//   - For each (state_id, address) currently "open" in the database
//     (delete_serial IS NULL) we compare to the parsed set:
//
//   - unchanged shape (same hash)  -> no-op
//
//   - shape changed (different hash) -> close the open row by
//     setting delete_serial = serial, then INSERT a new row with
//     create_serial = serial
//
//   - address absent from the parsed state -> close (only)
//
//   - For parsed addresses not previously open we INSERT a new row
//     with create_serial = serial.
//
// All work happens in the supplied transaction. raw_state on the
// state_versions row remains the authoritative round-trip form;
// resources is purely derived data.
func normalize(ctx context.Context, tx pgx.Tx, tenantID, stateID, stateVersionID string, serial int64, s *tfstate.State) error {
	parsed, err := buildResourceRows(stateID, serial, s)
	if err != nil {
		return fmt.Errorf("normalize resources: %w", err)
	}

	if err := applyResourceDelta(ctx, tx, tenantID, stateID, serial, parsed); err != nil {
		return fmt.Errorf("normalize resources: %w", err)
	}

	if err := insertOutputs(ctx, tx, tenantID, stateVersionID, s); err != nil {
		return fmt.Errorf("normalize outputs: %w", err)
	}
	return nil
}

// resourceRow is one row's worth of content as parsed from the
// incoming state, addressed by canonical Terraform address.
type resourceRow struct {
	address         string
	mode            string
	rtype           string
	name            string
	provider        string
	modulePath      string
	indexKind       string
	indexValue      any // nil for IndexNone, otherwise string
	schemaVersion   int
	attributes      string
	sensitivePaths  string
	dependenciesRaw string
	attributesHash  string
}

// buildResourceRows turns the parsed state into resourceRow values,
// computing the stable attributes_hash for each.
func buildResourceRows(stateID string, serial int64, s *tfstate.State) (map[string]resourceRow, error) {
	out := make(map[string]resourceRow, totalInstances(s))
	for _, r := range s.Resources {
		for _, inst := range r.Instances {
			addr, err := tfstate.InstanceAddress(r, inst)
			if err != nil {
				return nil, err
			}
			kind, val, err := inst.DecodeIndex()
			if err != nil {
				return nil, err
			}
			var indexVal any
			if kind != tfstate.IndexNone {
				indexVal = val
			}

			row := resourceRow{
				address:         addr,
				mode:            r.Mode,
				rtype:           r.Type,
				name:            r.Name,
				provider:        r.Provider,
				modulePath:      r.Module,
				indexKind:       kind.String(),
				indexValue:      indexVal,
				schemaVersion:   inst.SchemaVersion,
				attributes:      jsonOrDefault(inst.Attributes, "{}"),
				sensitivePaths:  jsonOrDefault(inst.SensitiveAttributes, "[]"),
				dependenciesRaw: marshalDependencies(inst.Dependencies),
			}
			row.attributesHash = hashResourceContent(row)
			out[addr] = row
		}
	}
	_ = stateID
	_ = serial
	return out, nil
}

// hashResourceContent computes a stable SHA-256 over the
// normalize-relevant content of a resource row. Two writes that
// produce identical normalized rows MUST produce identical hashes;
// changing the hash inputs is a breaking change that requires a
// reindex of the resources table.
//
// We do NOT canonicalize the JSON inputs (attributes, sensitive_paths,
// dependencies_raw): Terraform writes deterministic JSON (sorted map
// keys, no trailing whitespace), so byte-equal inputs yield byte-equal
// hashes in practice. Two semantically-equal but byte-different inputs
// would generate a spurious "shape changed" event -- acceptable as a
// false positive (one extra row); the alternative is parsing every
// attributes blob on every write, which is expensive.
func hashResourceContent(r resourceRow) string {
	var b strings.Builder
	const sep = "\x1f" // unit separator, never appears in JSON or addresses
	b.Grow(64 + len(r.attributes) + len(r.sensitivePaths) + len(r.dependenciesRaw))
	b.WriteString(r.mode)
	b.WriteString(sep)
	b.WriteString(r.rtype)
	b.WriteString(sep)
	b.WriteString(r.name)
	b.WriteString(sep)
	b.WriteString(r.provider)
	b.WriteString(sep)
	b.WriteString(r.modulePath)
	b.WriteString(sep)
	b.WriteString(r.indexKind)
	b.WriteString(sep)
	if r.indexValue != nil {
		if s, ok := r.indexValue.(string); ok {
			b.WriteString(s)
		} else {
			fmt.Fprintf(&b, "%v", r.indexValue)
		}
	}
	b.WriteString(sep)
	fmt.Fprintf(&b, "%d", r.schemaVersion)
	b.WriteString(sep)
	b.WriteString(r.attributes)
	b.WriteString(sep)
	b.WriteString(r.sensitivePaths)
	b.WriteString(sep)
	b.WriteString(r.dependenciesRaw)

	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// applyResourceDelta implements the lifecycle write algorithm. See
// the doc comment on normalize() for the contract.
func applyResourceDelta(
	ctx context.Context,
	tx pgx.Tx,
	tenantID, stateID string,
	serial int64,
	parsed map[string]resourceRow,
) error {
	open, err := loadOpenResources(ctx, tx, stateID)
	if err != nil {
		return fmt.Errorf("load open resources: %w", err)
	}

	toClose := make([]string, 0) // resources.id values to set delete_serial on
	toInsert := make([]resourceRow, 0)

	// Sweep parsed addresses: detect unchanged / changed / new.
	for addr, newRow := range parsed {
		existing, ok := open[addr]
		switch {
		case !ok:
			toInsert = append(toInsert, newRow)
		case existing.hash == newRow.attributesHash:
			// Unchanged shape; existing row stays open. Nothing to do.
		default:
			toClose = append(toClose, existing.id)
			toInsert = append(toInsert, newRow)
		}
	}

	// Sweep open addresses: anything not present in parsed is being deleted.
	for addr, existing := range open {
		if _, kept := parsed[addr]; !kept {
			toClose = append(toClose, existing.id)
		}
	}

	if err := closeResources(ctx, tx, toClose, serial); err != nil {
		return fmt.Errorf("close resources (%d): %w", len(toClose), err)
	}
	if err := insertResourceRows(ctx, tx, tenantID, stateID, serial, toInsert); err != nil {
		return fmt.Errorf("insert resources (%d): %w", len(toInsert), err)
	}
	return nil
}

func applyResourceDeltaSelected(
	ctx context.Context,
	tx pgx.Tx,
	tenantID, stateID string,
	serial int64,
	parsed map[string]resourceRow,
	selected []string,
) error {
	selected = dedupeSortedStrings(selected)
	if len(selected) == 0 {
		return nil
	}

	open, err := loadOpenResourcesByAddress(ctx, tx, stateID, selected)
	if err != nil {
		return fmt.Errorf("load selected open resources: %w", err)
	}

	toClose := make([]string, 0)
	toInsert := make([]resourceRow, 0)

	for _, addr := range selected {
		newRow, inParsed := parsed[addr]
		existing, inOpen := open[addr]
		switch {
		case inParsed && !inOpen:
			toInsert = append(toInsert, newRow)
		case inParsed && inOpen:
			if existing.hash != newRow.attributesHash {
				toClose = append(toClose, existing.id)
				toInsert = append(toInsert, newRow)
			}
		case !inParsed && inOpen:
			toClose = append(toClose, existing.id)
		}
	}

	if err := closeResources(ctx, tx, toClose, serial); err != nil {
		return fmt.Errorf("close selected resources (%d): %w", len(toClose), err)
	}
	if err := insertResourceRows(ctx, tx, tenantID, stateID, serial, toInsert); err != nil {
		return fmt.Errorf("insert selected resources (%d): %w", len(toInsert), err)
	}
	return nil
}

// openResource is the lightweight projection of a currently-open
// resources row needed to drive the delta computation.
type openResource struct {
	id   string
	hash string
}

type openResourceStateRow struct {
	address         string
	mode            string
	rtype           string
	name            string
	provider        string
	modulePath      string
	indexKind       string
	indexValue      string
	schemaVersion   int
	attributes      string
	sensitivePaths  string
	dependenciesRaw string
}

// loadOpenResources returns the set of open (delete_serial IS NULL)
// resources rows for the given state, indexed by address.
func loadOpenResources(ctx context.Context, tx pgx.Tx, stateID string) (map[string]openResource, error) {
	return loadOpenResourcesQuery(ctx, tx, stateID, "", nil)
}

func loadOpenResourcesByAddress(ctx context.Context, tx pgx.Tx, stateID string, addresses []string) (map[string]openResource, error) {
	if len(addresses) == 0 {
		return map[string]openResource{}, nil
	}
	return loadOpenResourcesQuery(ctx, tx, stateID, ` AND address = ANY($2::text[])`, []any{addresses})
}

func loadOpenResourcesQuery(ctx context.Context, tx pgx.Tx, stateID, suffix string, extraArgs []any) (map[string]openResource, error) {
	rows, err := tx.Query(ctx,
		`SELECT id, address, attributes_hash
		 FROM   resources
		 WHERE  state_id = $1 AND delete_serial IS NULL`+suffix,
		append([]any{stateID}, extraArgs...)...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]openResource)
	for rows.Next() {
		var id, addr, hash string
		if err := rows.Scan(&id, &addr, &hash); err != nil {
			return nil, err
		}
		out[addr] = openResource{id: id, hash: hash}
	}
	return out, rows.Err()
}

func loadOpenResourceStateRows(ctx context.Context, tx pgx.Tx, stateID string) ([]openResourceStateRow, error) {
	rows, err := tx.Query(ctx, `
		SELECT address, mode, type, name, provider, module_path,
		       index_kind, COALESCE(index_value, ''), schema_version, attributes::text,
		       sensitive_paths::text, dependencies_raw::text
		FROM   resources
		WHERE  state_id = $1
		  AND  delete_serial IS NULL
		ORDER  BY module_path, mode, type, name, provider, address
	`, stateID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]openResourceStateRow, 0)
	for rows.Next() {
		var item openResourceStateRow
		if err := rows.Scan(
			&item.address,
			&item.mode,
			&item.rtype,
			&item.name,
			&item.provider,
			&item.modulePath,
			&item.indexKind,
			&item.indexValue,
			&item.schemaVersion,
			&item.attributes,
			&item.sensitivePaths,
			&item.dependenciesRaw,
		); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func buildCurrentStateResourcesFromOpenRows(ctx context.Context, tx pgx.Tx, stateID string) ([]tfstate.Resource, error) {
	rows, err := loadOpenResourceStateRows(ctx, tx, stateID)
	if err != nil {
		return nil, fmt.Errorf("load open resource state rows: %w", err)
	}
	if len(rows) == 0 {
		return []tfstate.Resource{}, nil
	}

	type resourceKey struct {
		mode       string
		rtype      string
		name       string
		provider   string
		modulePath string
	}

	groupIndex := make(map[resourceKey]int, len(rows))
	resources := make([]tfstate.Resource, 0, len(rows))
	for _, row := range rows {
		key := resourceKey{
			mode:       row.mode,
			rtype:      row.rtype,
			name:       row.name,
			provider:   row.provider,
			modulePath: row.modulePath,
		}
		idx, ok := groupIndex[key]
		if !ok {
			idx = len(resources)
			groupIndex[key] = idx
			resources = append(resources, tfstate.Resource{
				Mode:      row.mode,
				Type:      row.rtype,
				Name:      row.name,
				Provider:  row.provider,
				Module:    row.modulePath,
				Instances: []tfstate.ResourceInstance{},
			})
		}
		inst, err := resourceInstanceFromOpenRow(row)
		if err != nil {
			return nil, err
		}
		resources[idx].Instances = append(resources[idx].Instances, inst)
	}
	return resources, nil
}

func resourceInstanceFromOpenRow(row openResourceStateRow) (tfstate.ResourceInstance, error) {
	inst := tfstate.ResourceInstance{
		SchemaVersion:       row.schemaVersion,
		Attributes:          json.RawMessage(row.attributes),
		SensitiveAttributes: json.RawMessage(row.sensitivePaths),
	}

	switch strings.TrimSpace(row.indexKind) {
	case "", "none":
	case "int":
		n, err := strconv.ParseInt(strings.TrimSpace(row.indexValue), 10, 64)
		if err != nil {
			return tfstate.ResourceInstance{}, fmt.Errorf("decode int index for %s: %w", row.address, err)
		}
		raw, err := json.Marshal(n)
		if err != nil {
			return tfstate.ResourceInstance{}, fmt.Errorf("encode int index for %s: %w", row.address, err)
		}
		inst.IndexKey = raw
	case "string":
		raw, err := json.Marshal(row.indexValue)
		if err != nil {
			return tfstate.ResourceInstance{}, fmt.Errorf("encode string index for %s: %w", row.address, err)
		}
		inst.IndexKey = raw
	default:
		return tfstate.ResourceInstance{}, fmt.Errorf("unsupported index kind %q for %s", row.indexKind, row.address)
	}

	if strings.TrimSpace(row.dependenciesRaw) != "" && strings.TrimSpace(row.dependenciesRaw) != "[]" {
		if err := json.Unmarshal([]byte(row.dependenciesRaw), &inst.Dependencies); err != nil {
			return tfstate.ResourceInstance{}, fmt.Errorf("decode dependencies for %s: %w", row.address, err)
		}
	}
	return inst, nil
}

// closeResources sets delete_serial on the given resource ids.
// Uses one statement with ANY($1) to keep this O(1) in round-trips
// regardless of how many resources are being closed.
func closeResources(ctx context.Context, tx pgx.Tx, ids []string, serial int64) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx,
		`UPDATE resources
		 SET    delete_serial = $1
		 WHERE  id = ANY($2::uuid[])
		   AND  delete_serial IS NULL`,
		serial, ids,
	)
	return err
}

// resourceColumns is the column list used by the COPY insert path.
// tenant_id is first so the binary-copy stream's column ordering
// matches the order we push values in below.
var resourceColumns = []string{
	"tenant_id", "state_id", "address", "mode", "type", "name", "provider",
	"module_path", "index_kind", "index_value", "schema_version",
	"attributes", "sensitive_paths", "dependencies_raw",
	"attributes_hash", "create_serial",
}

// insertResourceRows bulk-inserts the given rows using pgx.CopyFrom.
// tenantID, stateID and serial are folded into every row at copy time.
func insertResourceRows(
	ctx context.Context,
	tx pgx.Tx,
	tenantID, stateID string,
	serial int64,
	rows []resourceRow,
) error {
	if len(rows) == 0 {
		return nil
	}
	copyRows := make([][]any, 0, len(rows))
	for _, r := range rows {
		copyRows = append(copyRows, []any{
			tenantID,
			stateID,
			r.address,
			r.mode,
			r.rtype,
			r.name,
			r.provider,
			r.modulePath,
			r.indexKind,
			r.indexValue,
			r.schemaVersion,
			r.attributes,
			r.sensitivePaths,
			r.dependenciesRaw,
			r.attributesHash,
			serial,
		})
	}
	_, err := tx.CopyFrom(ctx,
		pgx.Identifier{"resources"},
		resourceColumns,
		pgx.CopyFromRows(copyRows),
	)
	return err
}

// insertOutputs writes one row per state-level output for this
// state_version. Outputs are still version-scoped in v0.x: counts are
// small per version and historical output values are useful as-is.
func insertOutputs(ctx context.Context, tx pgx.Tx, tenantID, stateVersionID string, s *tfstate.State) error {
	if len(s.Outputs) == 0 {
		return nil
	}
	rows := make([][]any, 0, len(s.Outputs))
	for name, out := range s.Outputs {
		rows = append(rows, []any{
			tenantID,
			stateVersionID,
			name,
			jsonOrDefault(out.Value, "null"),
			jsonOrDefault(out.Type, `"dynamic"`),
			out.Sensitive,
		})
	}
	_, err := tx.CopyFrom(ctx,
		pgx.Identifier{"outputs"},
		[]string{"tenant_id", "state_version_id", "name", "value", "value_type", "sensitive"},
		pgx.CopyFromRows(rows),
	)
	if err != nil {
		return fmt.Errorf("copy outputs (%d rows): %w", len(rows), err)
	}
	return nil
}

// totalInstances counts the expanded instance rows the state will
// project, so the resource map can be pre-sized.
func totalInstances(s *tfstate.State) int {
	n := 0
	for _, r := range s.Resources {
		n += len(r.Instances)
	}
	return n
}

// jsonOrDefault returns the raw JSON bytes as a string, falling back
// to the supplied default if raw is empty.
func jsonOrDefault(raw json.RawMessage, def string) string {
	if len(raw) == 0 {
		return def
	}
	return string(raw)
}

// marshalDependencies serializes the dependency address list as a
// JSONB array. A nil slice becomes the empty array so the column's
// NOT NULL DEFAULT is honored deterministically.
func marshalDependencies(deps []string) string {
	if len(deps) == 0 {
		return "[]"
	}
	b, _ := json.Marshal(deps)
	return string(b)
}

func dedupeSortedStrings(in []string) []string {
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
	slices.Sort(out)
	return out
}
