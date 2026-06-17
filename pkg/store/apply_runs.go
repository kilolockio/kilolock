package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kilolockio/kilolock/pkg/auth"
)

// ApplyRunStatus enumerates the lifecycle states of an apply_run.
// Mirrors the CHECK constraint on apply_runs.status (migration 0007).
//
// "committed" is the success terminal: a state_version was produced
// and the row-level merge succeeded. "failed" covers in-flight errors
// (terraform/provider returned a non-zero exit, merge violated a
// precondition, etc.). "aborted" is reserved for pre-emption — the
// reservation lease expired and another acquire reclaimed the rows,
// or the operator explicitly aborted via `kl apply abort`.
type ApplyRunStatus string

const (
	ApplyRunRunning   ApplyRunStatus = "running"
	ApplyRunCommitted ApplyRunStatus = "committed"
	ApplyRunFailed    ApplyRunStatus = "failed"
	ApplyRunAborted   ApplyRunStatus = "aborted"
)

// IsTerminal reports whether the status is one of the three end states.
func (s ApplyRunStatus) IsTerminal() bool {
	switch s {
	case ApplyRunCommitted, ApplyRunFailed, ApplyRunAborted:
		return true
	default:
		return false
	}
}

// ErrApplyRunNotFound is returned when a Get/Finish call is made for
// an id that does not exist.
var ErrApplyRunNotFound = errors.New("apply run not found")

// ErrApplyRunAlreadyFinished is returned by FinishApplyRun when the
// target row is already terminal. Re-finishing would silently
// overwrite the original outcome, which is almost never what the
// caller wants; surface the mistake instead.
var ErrApplyRunAlreadyFinished = errors.New("apply run already finished")

// ApplyRun is one row of apply_runs joined with the bookkeeping the
// v2 apply orchestrator records.
//
// Counter and timestamp fields are pointers because they are NULL
// while the run is in flight; reading them as plain ints would lose
// the "not yet populated" signal.
type ApplyRun struct {
	ID            string
	StateID       string
	FromVersionID string
	ToVersionID   *string

	SourceSerial    int64
	CommittedSerial *int64

	Actor *string

	StartedAt  time.Time
	FinishedAt *time.Time

	ResourcesPlanned *int
	ResourcesApplied *int
	ResourcesFailed  *int

	Status       ApplyRunStatus
	ErrorSummary *string

	Info      json.RawMessage
	CreatedAt time.Time
}

// BeginApplyRun records the start of an apply attempt and returns
// the new row.
//
// stateID and fromVersionID must reference real rows. sourceSerial
// is the trunk serial the plan was computed against (orchestrator
// uses it later to assert plan freshness). actor is free-form and
// optional. info may be nil; the column has a non-null default.
func (s *Store) BeginApplyRun(
	ctx context.Context,
	stateID, fromVersionID, actor string,
	sourceSerial int64,
	info json.RawMessage,
) (*ApplyRun, error) {
	if stateID == "" {
		return nil, errors.New("BeginApplyRun: stateID must not be empty")
	}
	if fromVersionID == "" {
		return nil, errors.New("BeginApplyRun: fromVersionID must not be empty")
	}
	if sourceSerial < 0 {
		return nil, errors.New("BeginApplyRun: sourceSerial must be non-negative")
	}
	if len(info) == 0 {
		// Honor the column's NOT NULL DEFAULT '{}' by sending an
		// explicit empty object rather than relying on the driver
		// to omit the param. Avoids a NULL/jsonb-mismatch surprise
		// if a future caller passes nil through a typed wrapper.
		info = json.RawMessage(`{}`)
	}

	tenantID := auth.TenantFromContext(ctx)
	const ins = `
		INSERT INTO apply_runs
			(tenant_id, state_id, from_version_id, source_serial, actor, info)
		VALUES
			($1, $2, $3, $4, NULLIF($5, ''), $6::jsonb)
		RETURNING id, state_id, from_version_id, source_serial,
		          started_at, status, info, created_at
	`
	var (
		run       ApplyRun
		statusStr string
	)
	err := s.pool.QueryRow(ctx, ins, tenantID, stateID, fromVersionID, sourceSerial, actor, string(info)).
		Scan(
			&run.ID, &run.StateID, &run.FromVersionID, &run.SourceSerial,
			&run.StartedAt, &statusStr, &run.Info, &run.CreatedAt,
		)
	if err != nil {
		return nil, fmt.Errorf("insert apply_runs: %w", err)
	}
	run.Status = ApplyRunStatus(statusStr)
	if actor != "" {
		a := actor
		run.Actor = &a
	}
	return &run, nil
}

// FinishApplyRunInput carries the terminal-state bookkeeping.
//
// CommittedSerial must be set for Status == ApplyRunCommitted, and
// must be nil otherwise. The apply_runs_committed_serial_only_on_commit
// CHECK constraint rejects the wrong combinations server-side, but
// this method validates client-side too so the error message points
// at the actual bug (rather than a generic constraint violation).
//
// ToVersionID may be empty if no state_version was written (failed /
// aborted before commit).
type FinishApplyRunInput struct {
	Status           ApplyRunStatus
	ToVersionID      string
	CommittedSerial  *int64
	ResourcesPlanned int
	ResourcesApplied int
	ResourcesFailed  int
	ErrorSummary     string
}

// FinishApplyRun marks the run terminal and stamps finished_at.
//
// Returns ErrApplyRunNotFound if no row matches id, and
// ErrApplyRunAlreadyFinished if the row exists but is already
// terminal.
func (s *Store) FinishApplyRun(ctx context.Context, id string, in FinishApplyRunInput) error {
	if id == "" {
		return errors.New("FinishApplyRun: id must not be empty")
	}
	if !in.Status.IsTerminal() {
		return fmt.Errorf("FinishApplyRun: status %q is not terminal", in.Status)
	}
	if in.Status == ApplyRunCommitted && in.CommittedSerial == nil {
		return errors.New("FinishApplyRun: committed status requires a committed_serial")
	}
	if in.Status != ApplyRunCommitted && in.CommittedSerial != nil {
		return errors.New("FinishApplyRun: committed_serial must be nil for non-committed status")
	}
	if in.ResourcesPlanned < 0 || in.ResourcesApplied < 0 || in.ResourcesFailed < 0 {
		return errors.New("FinishApplyRun: counter fields must be non-negative")
	}

	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var current string
		err := tx.QueryRow(ctx,
			`SELECT status FROM apply_runs WHERE id = $1 FOR UPDATE`,
			id,
		).Scan(&current)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrApplyRunNotFound
		}
		if err != nil {
			return fmt.Errorf("lock apply_runs: %w", err)
		}
		if ApplyRunStatus(current).IsTerminal() {
			return ErrApplyRunAlreadyFinished
		}

		const upd = `
			UPDATE apply_runs
			SET    status            = $2,
			       to_version_id     = NULLIF($3, '')::uuid,
			       committed_serial  = $4,
			       resources_planned = $5,
			       resources_applied = $6,
			       resources_failed  = $7,
			       error_summary     = NULLIF($8, ''),
			       finished_at       = now()
			WHERE  id = $1
		`
		_, err = tx.Exec(ctx, upd,
			id,
			string(in.Status),
			in.ToVersionID,
			in.CommittedSerial,
			in.ResourcesPlanned,
			in.ResourcesApplied,
			in.ResourcesFailed,
			in.ErrorSummary,
		)
		if err != nil {
			return fmt.Errorf("update apply_runs: %w", err)
		}
		return nil
	})
}

// GetApplyRun returns a single row by id, or ErrApplyRunNotFound.
func (s *Store) GetApplyRun(ctx context.Context, id string) (*ApplyRun, error) {
	if id == "" {
		return nil, errors.New("GetApplyRun: id must not be empty")
	}
	const q = `
		SELECT id, state_id, from_version_id, to_version_id,
		       source_serial, committed_serial,
		       actor,
		       started_at, finished_at,
		       resources_planned, resources_applied, resources_failed,
		       status, error_summary,
		       info, created_at
		FROM   apply_runs
		WHERE  id = $1
	`
	row := s.pool.QueryRow(ctx, q, id)
	out, err := scanApplyRun(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrApplyRunNotFound
	}
	return out, err
}

// ListApplyRuns returns the most recent apply_runs for a named state,
// newest first, capped at limit. limit <= 0 falls back to 100.
func (s *Store) ListApplyRuns(ctx context.Context, stateName string, limit int) ([]ApplyRun, error) {
	if stateName == "" {
		return nil, errors.New("ListApplyRuns: stateName must not be empty")
	}
	if limit <= 0 {
		limit = 100
	}

	where, args := s.stateByNameWhere(ctx, stateName)
	args = append(args, limit)
	limitParam := len(args)
	q := `
		SELECT a.id, a.state_id, a.from_version_id, a.to_version_id,
		       a.source_serial, a.committed_serial,
		       a.actor,
		       a.started_at, a.finished_at,
		       a.resources_planned, a.resources_applied, a.resources_failed,
		       a.status, a.error_summary,
		       a.info, a.created_at
		FROM   apply_runs a
		JOIN   states      s ON s.id = a.state_id
		WHERE  ` + where + `
		ORDER  BY a.started_at DESC
		LIMIT  $` + fmt.Sprint(limitParam)
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query apply_runs: %w", err)
	}
	defer rows.Close()

	var out []ApplyRun
	for rows.Next() {
		r, err := scanApplyRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate apply_runs: %w", err)
	}
	return out, nil
}

// GetApplyRunStatus returns the current status value for a run id.
// Returned error is ErrApplyRunNotFound if no row matches.
func (s *Store) GetApplyRunStatus(ctx context.Context, id string) (ApplyRunStatus, error) {
	if id == "" {
		return "", errors.New("GetApplyRunStatus: id must not be empty")
	}
	var statusStr string
	err := s.pool.QueryRow(ctx, `SELECT status FROM apply_runs WHERE id = $1`, id).Scan(&statusStr)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrApplyRunNotFound
	}
	if err != nil {
		return "", err
	}
	return ApplyRunStatus(statusStr), nil
}

// AbortApplyRun marks the run as aborted and stamps finished_at.
// It is safe to call multiple times: aborting a terminal run returns
// ErrApplyRunAlreadyFinished.
func (s *Store) AbortApplyRun(ctx context.Context, id, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "aborted by operator"
	}
	return s.FinishApplyRun(ctx, id, FinishApplyRunInput{
		Status:           ApplyRunAborted,
		ResourcesPlanned: 0,
		ResourcesApplied: 0,
		ResourcesFailed:  0,
		ErrorSummary:     reason,
	})
}

func scanApplyRun(r rowScanner) (*ApplyRun, error) {
	var (
		out       ApplyRun
		toVer     *string
		commitSer *int64
		actor     *string
		finished  *time.Time
		planned   *int
		applied   *int
		failed    *int
		statusStr string
		errSum    *string
	)
	err := r.Scan(
		&out.ID, &out.StateID, &out.FromVersionID, &toVer,
		&out.SourceSerial, &commitSer,
		&actor,
		&out.StartedAt, &finished,
		&planned, &applied, &failed,
		&statusStr, &errSum,
		&out.Info, &out.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	out.ToVersionID = toVer
	out.CommittedSerial = commitSer
	out.Actor = actor
	out.FinishedAt = finished
	out.ResourcesPlanned = planned
	out.ResourcesApplied = applied
	out.ResourcesFailed = failed
	out.Status = ApplyRunStatus(statusStr)
	out.ErrorSummary = errSum
	return &out, nil
}
