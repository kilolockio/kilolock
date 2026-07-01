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

const (
	stateEngineLockIDPrefix   = "state-engine-"
	stateEngineLockPathPrefix = "state-engine://"
	stateEngineLockVersion    = "state-engine/v1"
)

func isStateEngineLockPath(path string) bool {
	return strings.HasPrefix(strings.TrimSpace(path), stateEngineLockPathPrefix)
}

func stateEngineLockID(applyID string) string {
	return stateEngineLockIDPrefix + strings.TrimSpace(applyID)
}

func marshalStateEngineLockInfo(applyID string, scopeSummary []string) string {
	body := map[string]any{
		"kind":          "state_engine_write",
		"apply_id":      strings.TrimSpace(applyID),
		"scope_summary": scopeSummary,
	}
	b, err := json.Marshal(body)
	if err != nil {
		return fmt.Sprintf(`{"kind":"state_engine_write","apply_id":%q}`, strings.TrimSpace(applyID))
	}
	return string(b)
}

func scanExistingLock(ctx context.Context, tx pgx.Tx, stateID string) (LockInfo, bool, error) {
	var existing LockInfo
	err := tx.QueryRow(ctx,
		`SELECT lock_id, info, who, version, created, path
		 FROM   state_locks
		 WHERE  state_id = $1
		 ORDER  BY acquired_at ASC, lock_id ASC
		 LIMIT 1`,
		stateID,
	).Scan(
		&existing.ID, &existing.Info, &existing.Who,
		&existing.Version, &existing.Created, &existing.Path,
	)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return LockInfo{}, false, nil
	case err != nil:
		return LockInfo{}, false, err
	default:
		return existing, true, nil
	}
}

// AcquireStateEngineLock creates a Terraform-visible coarse lock that blocks
// plain Terraform/OpenTofu while a state-engine write is active.
func (s *Store) AcquireStateEngineLock(ctx context.Context, name, applyID, holder string, scopeSummary []string) (current LockInfo, err error) {
	tenantID := auth.TenantFromContext(ctx)
	lockID := stateEngineLockID(applyID)
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := enforceTenantLifecycleActive(ctx, tx, tenantID); err != nil {
			return err
		}
		stateID, err := upsertStateWithCreator(ctx, tx, tenantID, name, "", initialStateCreatorKL)
		if err != nil {
			return err
		}
		existing, ok, err := scanExistingLock(ctx, tx, stateID)
		if err != nil {
			return fmt.Errorf("probe existing coarse lock: %w", err)
		}
		if ok {
			if existing.ID == lockID && isStateEngineLockPath(existing.Path) {
				return nil
			}
			current = existing
			return ErrAlreadyLocked
		}

		info := LockInfo{
			ID:        lockID,
			Operation: "StateEngineWrite",
			Info:      marshalStateEngineLockInfo(applyID, scopeSummary),
			Who:       strings.TrimSpace(holder),
			Version:   stateEngineLockVersion,
			Created:   time.Now().UTC().Format(time.RFC3339Nano),
			Path:      stateEngineLockPathPrefix + strings.TrimSpace(name),
		}
		if strings.TrimSpace(info.Who) == "" {
			info.Who = "state-engine"
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO state_locks
				(tenant_id, state_id, lock_id, info, who, version, created, path, source_serial)
			 VALUES
			 	($1, $2, $3, $4, $5, $6, $7, $8, 0)`,
			tenantID, stateID, info.ID, info.Info, info.Who, info.Version, info.Created, info.Path,
		); err != nil {
			return fmt.Errorf("insert state-engine coarse lock: %w", err)
		}
		_, err = tx.Exec(ctx,
			`INSERT INTO events (kind, tenant_id, state_id, actor, payload)
			 VALUES ('lock_acquire', $1, $2, $3, jsonb_build_object('lock_id', $4::text, 'operation', $5::text))`,
			tenantID, stateID, info.Who, info.ID, info.Operation,
		)
		return err
	})
	return current, err
}

// ReleaseStateEngineLock clears the Terraform-visible coarse lock created by a
// state-engine write. It is idempotent.
func (s *Store) ReleaseStateEngineLock(ctx context.Context, name, applyID, actor string) error {
	tenantID := auth.TenantFromContext(ctx)
	lockID := stateEngineLockID(applyID)
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := enforceTenantLifecycleActive(ctx, tx, tenantID); err != nil {
			return err
		}
		where, args := s.statesByNameWhere(ctx, name)
		var stateID string
		err := tx.QueryRow(ctx, `SELECT id FROM states WHERE `+where, args...).Scan(&stateID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx,
			`DELETE FROM state_locks
			 WHERE state_id = $1 AND lock_id = $2 AND path LIKE $3`,
			stateID, lockID, stateEngineLockPathPrefix+"%",
		)
		if err != nil {
			return fmt.Errorf("delete state-engine coarse lock: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		if strings.TrimSpace(actor) == "" {
			actor = "state-engine"
		}
		_, err = tx.Exec(ctx,
			`INSERT INTO events (kind, tenant_id, state_id, actor, payload)
			 VALUES ('lock_release', $1, $2, $3, jsonb_build_object('lock_id', $4::text))`,
			tenantID, stateID, actor, lockID,
		)
		return err
	})
}
