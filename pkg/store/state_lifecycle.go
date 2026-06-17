package store

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/kilolockio/kilolock/pkg/auth"
)

func (s *Store) SetStateLifecycleStatusAudit(ctx context.Context, name string, status LifecycleStatus, actor, reason string) error {
	name = strings.TrimSpace(name)
	actor, reason = sanitizeLifecycleAudit(actor, reason)
	if name == "" {
		return ErrStateNotFound
	}
	if err := validateLifecycleTransitionAudit(status, reason); err != nil {
		return err
	}
	tenantID := ""
	if !s.isolated {
		tenantID = strings.TrimSpace(authTenantID(ctx))
		if tenantID == "" {
			return ErrStateNotFound
		}
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		where, args := s.statesByNameWhereAnyLifecycle(name, tenantID)
		var stateID, stateTenantID, finalName string
		if err := tx.QueryRow(ctx, `
SELECT id::text, tenant_id::text
FROM states
WHERE `+where+`
ORDER BY CASE WHEN name = $1 THEN 0 ELSE 1 END
LIMIT 1`, args...).Scan(&stateID, &stateTenantID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrStateNotFound
			}
			return err
		}
		finalName = name
		if status == LifecycleStatusArchived {
			finalName = archivedStateName(name)
		}
		q := `
UPDATE states
SET lifecycle_status = $2,
    name = $5,
    lifecycle_changed_at = now(),
    lifecycle_changed_by = $3,
    lifecycle_reason = $4,
    updated_at = now()
WHERE id = $1::uuid`
		tag, err := tx.Exec(ctx, q, stateID, string(status), actor, reason, finalName)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrStateNotFound
		}
		eventKind := "state_lifecycle_update"
		if status == LifecycleStatusArchived {
			eventKind = "state_archive"
		}
		if status == LifecycleStatusActive {
			eventKind = "state_restore"
		}
		_, err = tx.Exec(ctx,
			`INSERT INTO events (kind, tenant_id, state_id, actor, payload)
			 VALUES ($1, $2, $3, $4, jsonb_build_object('state', $5::text, 'final_state', $6::text, 'to', $7::text, 'reason', $8::text))`,
			eventKind, stateTenantID, stateID, actor, name, finalName, string(status), reason,
		)
		return err
	})
}

func (s *Store) statesByNameWhereAnyLifecycle(name, tenantID string) (string, []any) {
	if s.isolated {
		return "(name = $1 OR (lifecycle_status = 'archived' AND name LIKE $1 || '--archived-%'))", []any{name}
	}
	return "(name = $1 OR (lifecycle_status = 'archived' AND name LIKE $1 || '--archived-%')) AND tenant_id = $2", []any{name, tenantID}
}

func authTenantID(ctx context.Context) string {
	p, ok := auth.FromContext(ctx)
	if !ok {
		return ""
	}
	return p.TenantID
}

func (s *Store) GetStateLifecycleStatus(ctx context.Context, name string) (LifecycleStatus, error) {
	where, args := s.statesByNameWhereAnyLifecycle(strings.TrimSpace(name), authTenantID(ctx))
	var status string
	err := s.pool.QueryRow(ctx, `
SELECT lifecycle_status
FROM states
WHERE `+where+`
ORDER BY CASE WHEN name = $1 THEN 0 ELSE 1 END
LIMIT 1`, args...).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrStateNotFound
	}
	if err != nil {
		return "", err
	}
	return ParseLifecycleStatus(status)
}
