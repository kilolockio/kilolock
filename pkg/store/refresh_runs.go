package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kilolockio/kilolock/pkg/auth"
)

// RefreshRunStatus enumerates the terminal and in-flight states of a
// refresh run. It mirrors the CHECK constraint on refresh_runs.status
// (see migration 0005).
type RefreshRunStatus string

const (
	RefreshRunRunning   RefreshRunStatus = "running"
	RefreshRunSucceeded RefreshRunStatus = "succeeded"
	RefreshRunFailed    RefreshRunStatus = "failed"
	RefreshRunCancelled RefreshRunStatus = "cancelled"
)

// IsTerminal reports whether the status is one of the three end
// states. Useful for callers that want to assert "this run is done".
func (s RefreshRunStatus) IsTerminal() bool {
	switch s {
	case RefreshRunSucceeded, RefreshRunFailed, RefreshRunCancelled:
		return true
	default:
		return false
	}
}

// ErrRefreshRunNotFound is returned when a Get/Finish call is made
// for an id that does not exist. Callers should errors.Is on this
// rather than checking the message text.
var ErrRefreshRunNotFound = errors.New("refresh run not found")

// ErrRefreshRunAlreadyFinished is returned by FinishRefreshRun when
// the target row is already in a terminal state. Re-finishing would
// silently overwrite the original outcome, which is almost never
// what the caller wants; surface the mistake instead.
var ErrRefreshRunAlreadyFinished = errors.New("refresh run already finished")

// RefreshRun is one row of refresh_runs joined with the bookkeeping
// the orchestrator (v1.6b) needs.
//
// Counter fields are pointers because they are NULL while the run is
// in flight; reading them as plain ints would lose the "not yet
// populated" signal. Likewise FinishedAt and ToVersionID and
// ErrorSummary may be unset.
type RefreshRun struct {
	ID            string
	StateID       string
	FromVersionID string
	ToVersionID   *string

	StartedAt  time.Time
	FinishedAt *time.Time

	ResourcesChecked *int
	ResourcesChanged *int
	ResourcesFailed  *int

	Status       RefreshRunStatus
	ErrorSummary *string

	Actor     *string
	CreatedAt time.Time
}

// BeginRefreshRun records the start of a refresh attempt and returns
// the resulting row. The row begins in status='running' with
// FinishedAt and the counter fields unset; FinishRefreshRun is
// expected to be called exactly once afterwards.
//
// stateID and fromVersionID must reference real rows; the foreign
// keys will reject inputs that don't. actor is free-form and
// optional (empty string is allowed and is stored as NULL).
func (s *Store) BeginRefreshRun(ctx context.Context, stateID, fromVersionID, actor string) (*RefreshRun, error) {
	if stateID == "" {
		return nil, errors.New("BeginRefreshRun: stateID must not be empty")
	}
	if fromVersionID == "" {
		return nil, errors.New("BeginRefreshRun: fromVersionID must not be empty")
	}

	tenantID := auth.TenantFromContext(ctx)
	const ins = `
		INSERT INTO refresh_runs (tenant_id, state_id, from_version_id, actor)
		VALUES ($1, $2, $3, NULLIF($4, ''))
		RETURNING id, state_id, from_version_id, started_at, status, created_at
	`
	var (
		run       RefreshRun
		statusStr string
	)
	err := s.pool.QueryRow(ctx, ins, tenantID, stateID, fromVersionID, actor).
		Scan(&run.ID, &run.StateID, &run.FromVersionID, &run.StartedAt, &statusStr, &run.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("insert refresh_runs: %w", err)
	}
	run.Status = RefreshRunStatus(statusStr)
	if actor != "" {
		// Echo back the actor we just wrote so callers don't need to
		// re-read; matches the row we observe on a subsequent Get.
		a := actor
		run.Actor = &a
	}
	return &run, nil
}

// FinishRefreshRunInput carries the bookkeeping populated when a
// refresh run completes. ToVersionID may be empty (refresh found no
// drift, or failed before committing). ErrorSummary should be empty
// on success and a short operator-readable summary otherwise.
type FinishRefreshRunInput struct {
	Status           RefreshRunStatus
	ToVersionID      string
	ResourcesChecked int
	ResourcesChanged int
	ResourcesFailed  int
	ErrorSummary     string
}

// FinishRefreshRun marks the run terminal and stamps FinishedAt to
// now() (server-side). The CHECK constraint refresh_runs_running_has_no_finish
// enforces that a finished row also leaves status != 'running', so
// this method rejects status=='running' early.
//
// Returns ErrRefreshRunNotFound if no row matches id, and
// ErrRefreshRunAlreadyFinished if the row exists but is already
// terminal (re-finishing would silently overwrite the original
// outcome).
func (s *Store) FinishRefreshRun(ctx context.Context, id string, in FinishRefreshRunInput) error {
	if id == "" {
		return errors.New("FinishRefreshRun: id must not be empty")
	}
	if !in.Status.IsTerminal() {
		return fmt.Errorf("FinishRefreshRun: status %q is not terminal", in.Status)
	}
	if in.ResourcesChecked < 0 || in.ResourcesChanged < 0 || in.ResourcesFailed < 0 {
		return errors.New("FinishRefreshRun: counter fields must be non-negative")
	}

	// Two-step: precondition check + update. Wrapped in a single
	// transaction so the precondition can't race against another
	// finisher. Also makes ErrRefreshRunAlreadyFinished detectable
	// without inspecting tag.RowsAffected, which would tell us
	// nothing about whether a row exists at all.
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var current string
		err := tx.QueryRow(ctx,
			`SELECT status FROM refresh_runs WHERE id = $1 FOR UPDATE`,
			id,
		).Scan(&current)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrRefreshRunNotFound
		}
		if err != nil {
			return fmt.Errorf("lock refresh_runs: %w", err)
		}
		if RefreshRunStatus(current).IsTerminal() {
			return ErrRefreshRunAlreadyFinished
		}

		const upd = `
			UPDATE refresh_runs
			SET    status            = $2,
			       to_version_id     = NULLIF($3, '')::uuid,
			       resources_checked = $4,
			       resources_changed = $5,
			       resources_failed  = $6,
			       error_summary     = NULLIF($7, ''),
			       finished_at       = now()
			WHERE  id = $1
		`
		_, err = tx.Exec(ctx, upd,
			id,
			string(in.Status),
			in.ToVersionID,
			in.ResourcesChecked,
			in.ResourcesChanged,
			in.ResourcesFailed,
			in.ErrorSummary,
		)
		if err != nil {
			return fmt.Errorf("update refresh_runs: %w", err)
		}
		return nil
	})
}

// GetRefreshRun returns a single row by id. Returns
// ErrRefreshRunNotFound if no row matches.
func (s *Store) GetRefreshRun(ctx context.Context, id string) (*RefreshRun, error) {
	if id == "" {
		return nil, errors.New("GetRefreshRun: id must not be empty")
	}
	const q = `
		SELECT id, state_id, from_version_id, to_version_id,
		       started_at, finished_at,
		       resources_checked, resources_changed, resources_failed,
		       status, error_summary,
		       actor, created_at
		FROM   refresh_runs
		WHERE  id = $1
	`
	row := s.pool.QueryRow(ctx, q, id)
	out, err := scanRefreshRun(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrRefreshRunNotFound
	}
	return out, err
}

// ListRefreshRuns returns the most recent refresh runs for the named
// state, newest first, capped at limit. limit <= 0 falls back to a
// sensible default (100); the cap is enforced so callers can't
// accidentally pull every row in a long-running deployment.
func (s *Store) ListRefreshRuns(ctx context.Context, stateName string, limit int) ([]RefreshRun, error) {
	if stateName == "" {
		return nil, errors.New("ListRefreshRuns: stateName must not be empty")
	}
	if limit <= 0 {
		limit = 100
	}

	where, args := s.stateByNameWhere(ctx, stateName)
	args = append(args, limit)
	limitParam := len(args)
	q := `
		SELECT r.id, r.state_id, r.from_version_id, r.to_version_id,
		       r.started_at, r.finished_at,
		       r.resources_checked, r.resources_changed, r.resources_failed,
		       r.status, r.error_summary,
		       r.actor, r.created_at
		FROM   refresh_runs r
		JOIN   states         s ON s.id = r.state_id
		WHERE  ` + where + `
		ORDER  BY r.started_at DESC
		LIMIT  $` + fmt.Sprint(limitParam)
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query refresh_runs: %w", err)
	}
	defer rows.Close()

	var out []RefreshRun
	for rows.Next() {
		r, err := scanRefreshRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate refresh_runs: %w", err)
	}
	return out, nil
}

// rowScanner is the common surface implemented by both pgx.Row and
// pgx.Rows that the scan helper needs.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanRefreshRun(r rowScanner) (*RefreshRun, error) {
	var (
		out       RefreshRun
		toVer     *string
		finished  *time.Time
		checked   *int
		changed   *int
		failed    *int
		statusStr string
		errSum    *string
		actor     *string
	)
	err := r.Scan(
		&out.ID, &out.StateID, &out.FromVersionID, &toVer,
		&out.StartedAt, &finished,
		&checked, &changed, &failed,
		&statusStr, &errSum,
		&actor, &out.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	out.ToVersionID = toVer
	out.FinishedAt = finished
	out.ResourcesChecked = checked
	out.ResourcesChanged = changed
	out.ResourcesFailed = failed
	out.Status = RefreshRunStatus(statusStr)
	out.ErrorSummary = errSum
	out.Actor = actor
	return &out, nil
}
