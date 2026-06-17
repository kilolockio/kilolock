package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// StateStatus is the consolidated answer to "what's happening with
// my state right now?" — the operator-side replacement for ad-hoc
// psql queries when an apply seems stuck. Returned by
// GetStateStatus, rendered by the CLI's `kl status`.
//
// Every field is scoped to one state (resolved by name within the
// caller's tenant) and represents the live picture at the moment
// of the call. Nothing is cached.
type StateStatus struct {
	// State identity / current version. UpdatedAt is wall-clock
	// time of the most recent state-touching event (write,
	// lock acquire/release, rollback).
	Name             string
	Lineage          string
	CurrentSerial    int64
	TerraformVersion string
	UpdatedAt        time.Time
	ResourceCount    int
	ExclusiveLocks   bool
	CoexistenceMode  StateCoexistenceMode

	// Locks lists the active v1 HTTP-backend whole-state locks held
	// by vanilla Terraform clients. In optimistic mode there may be
	// multiple rows; in exclusive mode there is at most one. The v2
	// apply orchestrator does NOT use these locks.
	Locks []StatusLock

	// Lock is the first element of Locks when any exist, kept for
	// compatibility with older callers that only expected a single
	// lock row. New code should prefer Locks.
	Lock *StatusLock

	// InFlightApplies are apply_runs rows whose status is still
	// 'running'. Sorted newest-first. Each row carries enough
	// metadata for the operator to decide whether to wait, kill,
	// or investigate.
	InFlightApplies []StatusApplyRun

	// ActiveReservations are non-expired reservation rows for
	// this state, sorted by (address, mode). The expected
	// non-empty cardinality is "one orchestrator's working
	// set"; multiple-orchestrator overlap shows up here as
	// rows from different apply_run ids and is the right thing
	// to surface to the operator looking at why their own
	// apply is queued.
	ActiveReservations []ActiveReservation
}

// StatusLock is the projection of state_locks used in StateStatus.
// Kept separate from LockInfo (the wire-format struct used by the
// Terraform protocol) so we can present timestamps as time.Time
// rather than the protocol's string form and so future fields
// (lease deadlines, etc.) can land without breaking the wire type.
//
// Operation/Info: the Terraform-protocol "operation" field (apply,
// plan, etc.) is not stored as its own column; it arrives encoded
// inside the `info` text field as JSON. The CLI renderer surfaces
// `info` directly rather than parsing it — operators are used to
// reading raw LockInfo and the parse risk isn't worth the marginal
// readability gain.
type StatusLock struct {
	LockID  string
	Who     string
	Version string
	Path    string
	Info    string
	Created time.Time
}

// StatusApplyRun is the projection of apply_runs used for the
// in-flight list. Same separation rationale as StatusLock.
type StatusApplyRun struct {
	ID               string
	Actor            string
	SourceSerial     int64
	StartedAt        time.Time
	ResourcesPlanned *int
	ResourcesApplied *int
	ResourcesFailed  *int
}

// GetStateStatus returns the live status of the named state for the
// caller's tenant. Returns ErrStateNotFound if the state does not
// exist (in this tenant — another tenant with the same name does
// not satisfy the lookup).
//
// Implementation: one round trip per concern (state, lock, applies,
// reservations). The alternative is a single multi-CTE query
// returning JSON aggregates, which is faster but considerably
// harder to read; status is rendered interactively and the latency
// budget is generous (target <100ms; current measured ~15ms).
func (s *Store) GetStateStatus(ctx context.Context, name string) (*StateStatus, error) {
	if name == "" {
		return nil, errors.New("GetStateStatus: name must not be empty")
	}
	// Step 1: resolve the state row + current version metadata.
	// One LEFT JOIN handles the "state exists but has no
	// versions" edge case (impossible today, but defensive — we
	// surface zero serial / empty lineage rather than nil-pointer
	// panicking).
	where, args := s.stateByNameWhere(ctx, name)
	stateQ := `
		SELECT s.id, s.name,
		       COALESCE(s.lineage::text, '')          AS lineage,
		       COALESCE(sv.serial, 0)                 AS serial,
		       COALESCE(sv.terraform_version, '')     AS tf_version,
		       s.exclusive_locks,
		       s.coexistence_mode,
		       s.updated_at,
		       COALESCE((SELECT COUNT(*) FROM resources r
		                 WHERE r.state_id = s.id AND r.mode = 'managed' AND r.delete_serial IS NULL), 0) AS resource_count
		FROM   states s
		LEFT   JOIN state_versions sv ON sv.id = s.current_version_id
		WHERE  ` + where
	var (
		out     StateStatus
		stateID string
	)
	err := s.pool.QueryRow(ctx, stateQ, args...).Scan(
		&stateID, &out.Name, &out.Lineage,
		&out.CurrentSerial, &out.TerraformVersion,
		&out.ExclusiveLocks, &out.CoexistenceMode,
		&out.UpdatedAt, &out.ResourceCount,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrStateNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("query state status: %w", err)
	}

	// Step 2: v1 whole-state locks (if any). In optimistic mode there
	// may be multiple concurrent rows for one state, so we return the
	// full list. `created` is a Terraform-protocol-supplied STRING, not
	// a timestamptz; we present acquired_at (the database wall-clock at
	// INSERT time) because it's the trustworthy reference.
	const lockQ = `
		SELECT lock_id,
		       COALESCE(who, ''),
		       COALESCE(version, ''),
		       COALESCE(path, ''),
		       COALESCE(info, ''),
		       acquired_at
		FROM   state_locks
		WHERE  state_id = $1
		ORDER  BY acquired_at ASC, lock_id ASC
	`
	lockRows, err := s.pool.Query(ctx, lockQ, stateID)
	if err != nil {
		return nil, fmt.Errorf("query state locks: %w", err)
	}
	for lockRows.Next() {
		var lk StatusLock
		if err := lockRows.Scan(
			&lk.LockID, &lk.Who,
			&lk.Version, &lk.Path, &lk.Info, &lk.Created,
		); err != nil {
			lockRows.Close()
			return nil, fmt.Errorf("scan state lock: %w", err)
		}
		out.Locks = append(out.Locks, lk)
	}
	lockRows.Close()
	if err := lockRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate state locks: %w", err)
	}
	if len(out.Locks) > 0 {
		out.Lock = &out.Locks[0]
	}

	// Step 3: in-flight applies. Status filter is on the running
	// constant, not free-form, so a future status enum change
	// would touch this file as a compile-time error. ORDER BY
	// started_at DESC so the most recent attempt is at the top
	// of the operator's screen.
	const applyQ = `
		SELECT id, COALESCE(actor, ''),
		       source_serial, started_at,
		       resources_planned, resources_applied, resources_failed
		FROM   apply_runs
		WHERE  state_id = $1 AND status = 'running'
		ORDER  BY started_at DESC
	`
	rows, err := s.pool.Query(ctx, applyQ, stateID)
	if err != nil {
		return nil, fmt.Errorf("query in-flight applies: %w", err)
	}
	for rows.Next() {
		var ar StatusApplyRun
		if err := rows.Scan(
			&ar.ID, &ar.Actor,
			&ar.SourceSerial, &ar.StartedAt,
			&ar.ResourcesPlanned, &ar.ResourcesApplied, &ar.ResourcesFailed,
		); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan in-flight apply: %w", err)
		}
		out.InFlightApplies = append(out.InFlightApplies, ar)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate in-flight applies: %w", err)
	}

	// Step 4: active reservations. Mirrors ListActiveReservations
	// but skips the JOIN to states (we already have state_id).
	// expires_at filter is server-side so any expired-but-not-
	// yet-reaped rows don't appear as "still held".
	const resQ = `
		SELECT id, state_id, apply_id, address_glob,
		       mode, holder, acquired_at, expires_at
		FROM   resource_reservations
		WHERE  state_id = $1 AND expires_at >= now()
		ORDER  BY address_glob, mode
	`
	resRows, err := s.pool.Query(ctx, resQ, stateID)
	if err != nil {
		return nil, fmt.Errorf("query active reservations: %w", err)
	}
	for resRows.Next() {
		var (
			r       ActiveReservation
			modeStr string
		)
		if err := resRows.Scan(
			&r.ID, &r.StateID, &r.ApplyID, &r.AddressGlob,
			&modeStr, &r.Holder, &r.AcquiredAt, &r.ExpiresAt,
		); err != nil {
			resRows.Close()
			return nil, fmt.Errorf("scan reservation: %w", err)
		}
		r.Mode = ReservationMode(modeStr)
		out.ActiveReservations = append(out.ActiveReservations, r)
	}
	resRows.Close()
	if err := resRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate reservations: %w", err)
	}

	return &out, nil
}
