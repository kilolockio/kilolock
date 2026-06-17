package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// DriftedResource is one row from the current_resource_drift view.
// It is the operator-facing answer to "what is currently drifted in
// this state, and what changed?" — the demo deliverable for v1.7.
//
// CurrentAttributes is what the cloud reported on the most recent
// refresh; PreviousAttributes is what was stored immediately before
// that refresh. Both are the raw JSONB blobs as written through the
// lifecycle; the caller decides whether to diff structurally or
// hand them off to a UI.
type DriftedResource struct {
	ResourceID          string
	StateID             string
	StateName           string
	Address             string
	Type                string
	Mode                string
	ModulePath          string
	CurrentAttributes   json.RawMessage
	PreviousAttributes  json.RawMessage
	DetectedAtSerial    int64
	DetectedInVersionID string
	DetectedAt          time.Time
	// RefreshRunID is the audit row that produced the new version.
	// May be nil if the audit row was pruned but the state_version
	// is still around.
	RefreshRunID *string
}

// ListCurrentDrift returns the drift view rows for one state,
// ordered by detected_at_serial DESC (newest drift first) then
// address ASC for diff-stable output.
//
// limit <= 0 falls back to a generous default (1000). Callers
// expecting more than that should page; the view itself is
// indexed by (state_id, address, delete_serial) so pagination
// stays cheap.
//
// Returns (nil, ErrStateNotFound) when the state does not exist —
// distinguishing "no drift" (empty slice, nil error) from "no such
// state" (which the demo script wants to surface clearly).
func (s *Store) ListCurrentDrift(ctx context.Context, stateName string, limit int) ([]DriftedResource, error) {
	if stateName == "" {
		return nil, errors.New("ListCurrentDrift: stateName must not be empty")
	}
	if limit <= 0 {
		limit = 1000
	}

	// Sanity-check the state exists before running the view query
	// so empty result + "no such state" are not conflated.
	where, args := s.statesByNameWhere(ctx, stateName)
	var stateID string
	err := s.pool.QueryRow(ctx,
		`SELECT id FROM states WHERE `+where,
		args...,
	).Scan(&stateID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrStateNotFound
		}
		return nil, fmt.Errorf("lookup state %q: %w", stateName, err)
	}

	const q = `
		SELECT resource_id, state_id, state_name,
		       address, type, mode, module_path,
		       current_attributes, previous_attributes,
		       detected_at_serial, detected_in_version_id,
		       detected_at, refresh_run_id
		FROM   current_resource_drift
		WHERE  state_id = $1
		ORDER  BY detected_at_serial DESC, address ASC
		LIMIT  $2
	`
	rows, err := s.pool.Query(ctx, q, stateID, limit)
	if err != nil {
		return nil, fmt.Errorf("query current_resource_drift: %w", err)
	}
	defer rows.Close()

	out := make([]DriftedResource, 0)
	for rows.Next() {
		var (
			r            DriftedResource
			runID        *string
			curAttr      []byte
			prevAttr     []byte
			detectedAtSV string
			detectedAtTS time.Time
		)
		if err := rows.Scan(
			&r.ResourceID, &r.StateID, &r.StateName,
			&r.Address, &r.Type, &r.Mode, &r.ModulePath,
			&curAttr, &prevAttr,
			&r.DetectedAtSerial, &detectedAtSV,
			&detectedAtTS, &runID,
		); err != nil {
			return nil, fmt.Errorf("scan drift row: %w", err)
		}
		r.CurrentAttributes = json.RawMessage(curAttr)
		r.PreviousAttributes = json.RawMessage(prevAttr)
		r.DetectedInVersionID = detectedAtSV
		r.DetectedAt = detectedAtTS
		r.RefreshRunID = runID
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate current_resource_drift: %w", err)
	}
	return out, nil
}
