package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/kilolockio/kilolock/pkg/auth"
)

// ErrAlreadyLocked is returned from AcquireLock when a state already has a
// lock held by a different owner. The current LockInfo is returned
// alongside so the caller can show it to the user (matches Terraform's
// expectation that 423 responses carry the conflicting lock info).
var ErrAlreadyLocked = errors.New("state already locked")

// AcquireLock attempts to take a lock on the named state, creating the
// state row on first use. On success, it returns nil. On conflict, it
// returns ErrAlreadyLocked and populates current with the existing lock
// information.
//
// Concurrency model (migration 0011):
//
//   - When states.exclusive_locks is true, behavior is the legacy
//     one-writer-at-a-time model: a second AcquireLock for the same
//     state returns ErrAlreadyLocked.
//
//   - When states.exclusive_locks is false (the default), multiple
//     operators may each hold their own lock concurrently. Each lock
//     row records the trunk serial visible at acquire time as
//     `source_serial`; the optimistic POST path uses it to scope
//     conflict detection to "what committed since this operator
//     started." The actual conflict arbitration happens inside
//     WriteState, not here.
//
// In optimistic mode the only conflict AcquireLock will surface is a
// duplicate lock_id collision — itself extremely unlikely (Terraform
// generates lock_ids as fresh UUIDs) and indicative of a buggy
// client, not normal contention.
func (s *Store) AcquireLock(ctx context.Context, name string, info LockInfo) (current LockInfo, err error) {
	tenantID := auth.TenantFromContext(ctx)
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := enforceTenantLifecycleActive(ctx, tx, tenantID); err != nil {
			return err
		}
		stateID, err := upsertState(ctx, tx, tenantID, name, "")
		if err != nil {
			return err
		}

		// Read the per-state exclusive_locks flag and the trunk
		// serial in one round-trip. trunk serial defaults to 0
		// (genesis) for a brand-new state that has no versions yet.
		var (
			exclusive    bool
			sourceSerial int64
		)
		err = tx.QueryRow(ctx,
			`SELECT s.exclusive_locks,
			        COALESCE(sv.serial, 0)
			 FROM   states s
			 LEFT JOIN state_versions sv ON sv.id = s.current_version_id
			 WHERE  s.id = $1`,
			stateID,
		).Scan(&exclusive, &sourceSerial)
		if err != nil {
			return fmt.Errorf("read lock context: %w", err)
		}

		// Exclusive mode: reject if any lock is already held.
		if exclusive {
			var existing LockInfo
			err := tx.QueryRow(ctx,
				`SELECT lock_id, info, who, version, created, path
				 FROM   state_locks
				 WHERE  state_id = $1
				 LIMIT 1`,
				stateID,
			).Scan(
				&existing.ID, &existing.Info, &existing.Who,
				&existing.Version, &existing.Created, &existing.Path,
			)
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				// fall through to insert
			case err != nil:
				return fmt.Errorf("probe existing lock: %w", err)
			default:
				current = existing
				return ErrAlreadyLocked
			}
		}

		// Optimistic mode (or exclusive-with-no-existing-lock): insert
		// a new lock row. The (state_id, lock_id) PK from migration
		// 0011 lets multiple rows coexist in optimistic mode, while
		// still catching the unlikely client bug of replaying the same
		// lock_id twice.
		const insertSQL = `
			INSERT INTO state_locks
				(tenant_id, state_id, lock_id, info, who, version, created, path, source_serial)
			VALUES
				($1, $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT (state_id, lock_id) DO NOTHING
		`
		tag, err := tx.Exec(ctx, insertSQL,
			tenantID, stateID, info.ID, info.Info, info.Who, info.Version, info.Created, info.Path, sourceSerial,
		)
		if err != nil {
			return fmt.Errorf("insert state_lock: %w", err)
		}
		if tag.RowsAffected() == 1 {
			_, err = tx.Exec(ctx,
				`INSERT INTO events (kind, tenant_id, state_id, actor, payload)
				 VALUES ('lock_acquire', $1, $2, $3, jsonb_build_object('lock_id', $4::text, 'operation', $5::text, 'source_serial', $6::bigint))`,
				tenantID, stateID, info.Who, info.ID, info.Operation, sourceSerial,
			)
			return err
		}

		// Insert was a no-op: the same lock_id was already present.
		// Treat this as a duplicate of the existing row — return the
		// existing row's info so the client sees Terraform's familiar
		// 423 + lock info response.
		const selectSQL = `
			SELECT lock_id, info, who, version, created, path
			FROM   state_locks
			WHERE  state_id = $1 AND lock_id = $2
		`
		var got LockInfo
		err = tx.QueryRow(ctx, selectSQL, stateID, info.ID).Scan(
			&got.ID, &got.Info, &got.Who, &got.Version, &got.Created, &got.Path,
		)
		if err != nil {
			return fmt.Errorf("read existing lock: %w", err)
		}
		current = got
		return ErrAlreadyLocked
	})
	return current, err
}

// ReleaseLock releases a held lock, but only if the lockID matches. If no
// lock is held or the ID does not match, ErrLockMismatch is returned.
func (s *Store) ReleaseLock(ctx context.Context, name, lockID, actor string) error {
	tenantID := auth.TenantFromContext(ctx)
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := enforceTenantLifecycleActive(ctx, tx, tenantID); err != nil {
			return err
		}
		where, args := s.statesByNameWhere(ctx, name)
		var stateID string
		err := tx.QueryRow(ctx,
			`SELECT id FROM states WHERE `+where,
			args...,
		).Scan(&stateID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrLockMismatch
		}
		if err != nil {
			return err
		}

		tag, err := tx.Exec(ctx,
			`DELETE FROM state_locks WHERE state_id = $1 AND lock_id = $2`,
			stateID, lockID,
		)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrLockMismatch
		}

		_, err = tx.Exec(ctx,
			`INSERT INTO events (kind, tenant_id, state_id, actor, payload)
			 VALUES ('lock_release', $1, $2, $3, jsonb_build_object('lock_id', $4::text))`,
			tenantID, stateID, actor, lockID,
		)
		return err
	})
}

// ForceReleaseLock unconditionally clears any lock held on the named
// state. It exists because Terraform's `terraform force-unlock` issues
// an UNLOCK request with an empty body and never transmits the
// user-supplied lock ID over the wire (see Terraform's
// internal/backend/remote-state/http/client.go Unlock): for an
// out-of-process force-unlock c.jsonLockInfo is nil, so the request
// body is empty. The administrative semantics are exactly
// "release whatever is held" -- we record it as a separate event kind
// so the audit trail can distinguish an owner-driven release from an
// administrative override.
//
// Idempotent: if no lock is held (or the state doesn't exist at all)
// the operation succeeds without error.
func (s *Store) ForceReleaseLock(ctx context.Context, name, actor string) error {
	tenantID := auth.TenantFromContext(ctx)
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := enforceTenantLifecycleActive(ctx, tx, tenantID); err != nil {
			return err
		}
		where, args := s.statesByNameWhere(ctx, name)
		var stateID string
		err := tx.QueryRow(ctx,
			`SELECT id FROM states WHERE `+where,
			args...,
		).Scan(&stateID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}

		var releasedID *string
		err = tx.QueryRow(ctx,
			`DELETE FROM state_locks WHERE state_id = $1
			 RETURNING lock_id`,
			stateID,
		).Scan(&releasedID)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return nil
		case err != nil:
			return fmt.Errorf("delete state_lock: %w", err)
		}

		var lockID string
		if releasedID != nil {
			lockID = *releasedID
		}
		_, err = tx.Exec(ctx,
			`INSERT INTO events (kind, tenant_id, state_id, actor, payload)
			 VALUES ('lock_force_release', $1, $2, $3, jsonb_build_object('lock_id', $4::text))`,
			tenantID, stateID, actor, lockID,
		)
		return err
	})
}
