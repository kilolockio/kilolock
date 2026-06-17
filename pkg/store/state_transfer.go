package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// MoveEnvironmentStateNamespace rehomes every state under one environment from
// source workspace/tenant ownership to target workspace/tenant ownership.
//
// It rewrites the leading state path namespace from:
//
//	sourceWorkspaceID/envPublicID/...
//
// to:
//
//	targetWorkspaceID/envPublicID/...
//
// and updates tenant_id across all tenant-scoped data-plane tables that follow
// a state row.
func (s *Store) MoveEnvironmentStateNamespace(ctx context.Context, sourceTenantID, targetTenantID, sourceWorkspaceID, targetWorkspaceID, envPublicID string) error {
	sourceTenantID = strings.TrimSpace(sourceTenantID)
	targetTenantID = strings.TrimSpace(targetTenantID)
	sourceWorkspaceID = strings.TrimSpace(sourceWorkspaceID)
	targetWorkspaceID = strings.TrimSpace(targetWorkspaceID)
	envPublicID = strings.TrimSpace(envPublicID)
	if sourceTenantID == "" || targetTenantID == "" || sourceWorkspaceID == "" || targetWorkspaceID == "" || envPublicID == "" {
		return fmt.Errorf("source tenant, target tenant, source workspace, target workspace, and environment id are required")
	}
	oldPrefix := sourceWorkspaceID + "/" + envPublicID + "/"
	newPrefix := targetWorkspaceID + "/" + envPublicID + "/"

	type moveRow struct {
		ID      string
		OldName string
		NewName string
	}
	type duplicateCleanup struct {
		sourceStateID string
	}
	var moving []moveRow
	rows, err := s.pool.Query(ctx, `
SELECT id::text, name
FROM states
WHERE tenant_id = $1
  AND name LIKE $2
ORDER BY name`, sourceTenantID, oldPrefix+"%")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return err
		}
		moving = append(moving, moveRow{
			ID:      id,
			OldName: name,
			NewName: newPrefix + strings.TrimPrefix(name, oldPrefix),
		})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(moving) == 0 {
		return nil
	}

	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
UPDATE environments
SET tenant_id = $2,
    updated_at = now()
WHERE tenant_id = $1
  AND env_public_id = $3`, sourceTenantID, targetTenantID, envPublicID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
UPDATE api_tokens
SET tenant_id = $2
WHERE tenant_id = $1
  AND environment_id IN (
      SELECT id
      FROM environments
      WHERE tenant_id = $2
        AND env_public_id = $3
  )`, sourceTenantID, targetTenantID, envPublicID); err != nil {
			return err
		}

		var duplicateCleanups []duplicateCleanup
		for _, item := range moving {
			var (
				targetStateID string
				targetLineage string
				targetRaw     string
			)
			err := tx.QueryRow(ctx, `
SELECT s.id::text, COALESCE(s.lineage::text, ''), COALESCE(sv.raw_state::text, '')
FROM states s
LEFT JOIN state_versions sv ON sv.id = s.current_version_id
WHERE s.tenant_id = $1
  AND s.name = $2
  AND s.id <> $3::uuid`, targetTenantID, item.NewName, item.ID).Scan(&targetStateID, &targetLineage, &targetRaw)
			if err != nil && err != pgx.ErrNoRows {
				return err
			}
			if err == nil {
				var (
					sourceLineage string
					sourceRaw     string
				)
				if err := tx.QueryRow(ctx, `
SELECT COALESCE(s.lineage::text, ''), COALESCE(sv.raw_state::text, '')
FROM states s
LEFT JOIN state_versions sv ON sv.id = s.current_version_id
WHERE s.id = $1::uuid`, item.ID).Scan(&sourceLineage, &sourceRaw); err != nil {
					return err
				}
				if sourceLineage == targetLineage && sourceRaw == targetRaw {
					duplicateCleanups = append(duplicateCleanups, duplicateCleanup{sourceStateID: item.ID})
					continue
				}
				return fmt.Errorf("target workspace already has a state named %q", item.NewName)
			}
		}
		for _, item := range moving {
			skip := false
			for _, dup := range duplicateCleanups {
				if dup.sourceStateID == item.ID {
					skip = true
					break
				}
			}
			if skip {
				continue
			}
			if _, err := tx.Exec(ctx, `
UPDATE states
SET tenant_id = $2,
    name = $3,
    updated_at = now()
WHERE id = $1::uuid`, item.ID, targetTenantID, item.NewName); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE state_versions SET tenant_id = $2 WHERE state_id = $1::uuid`, item.ID, targetTenantID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE resources SET tenant_id = $2 WHERE state_id = $1::uuid`, item.ID, targetTenantID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
UPDATE outputs
SET tenant_id = $2
WHERE state_version_id IN (SELECT id FROM state_versions WHERE state_id = $1::uuid)`, item.ID, targetTenantID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE state_locks SET tenant_id = $2 WHERE state_id = $1::uuid`, item.ID, targetTenantID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE events SET tenant_id = $2 WHERE state_id = $1::uuid`, item.ID, targetTenantID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE refresh_runs SET tenant_id = $2 WHERE state_id = $1::uuid`, item.ID, targetTenantID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE apply_runs SET tenant_id = $2 WHERE state_id = $1::uuid`, item.ID, targetTenantID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE resource_reservations SET tenant_id = $2 WHERE state_id = $1::uuid`, item.ID, targetTenantID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE state_version_tags SET tenant_id = $2 WHERE state_id = $1::uuid`, item.ID, targetTenantID); err != nil {
				return err
			}
		}
		for _, dup := range duplicateCleanups {
			if _, err := tx.Exec(ctx, `DELETE FROM states WHERE id = $1::uuid`, dup.sourceStateID); err != nil {
				return err
			}
		}
		return nil
	})
}
