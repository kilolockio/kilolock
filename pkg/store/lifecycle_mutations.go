package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

func sanitizeLifecycleAudit(actor, reason string) (string, string) {
	actor = strings.TrimSpace(actor)
	reason = strings.TrimSpace(reason)
	return actor, reason
}

func validateLifecycleTransitionAudit(status LifecycleStatus, reason string) error {
	switch status {
	case LifecycleStatusSuspended, LifecycleStatusArchived:
		if strings.TrimSpace(reason) == "" {
			return fmt.Errorf("reason is required for lifecycle status %q", status)
		}
	}
	return nil
}

func (s *Store) SetTenantLifecycleStatus(ctx context.Context, slug string, status LifecycleStatus) error {
	return s.SetTenantLifecycleStatusAudit(ctx, slug, status, "", "")
}

func (s *Store) SetTenantLifecycleStatusAudit(ctx context.Context, slug string, status LifecycleStatus, actor, reason string) error {
	actor, reason = sanitizeLifecycleAudit(actor, reason)
	if err := validateLifecycleTransitionAudit(status, reason); err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var (
			tenantID  string
			oldStatus string
		)
		err := tx.QueryRow(ctx, `
WITH current_row AS (
	SELECT id, lifecycle_status
	FROM tenants
	WHERE slug = $1
	FOR UPDATE
)
UPDATE tenants t
SET lifecycle_status = $2,
    lifecycle_changed_at = now(),
    lifecycle_changed_by = $3,
    lifecycle_reason = $4,
    updated_at = now()
FROM current_row c
WHERE t.id = c.id
RETURNING t.id::text, c.lifecycle_status
`, strings.TrimSpace(slug), string(status), actor, reason).Scan(&tenantID, &oldStatus)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrTenantNotFound
			}
			return err
		}
		payload, err := json.Marshal(map[string]any{
			"slug":       strings.TrimSpace(slug),
			"from":       oldStatus,
			"to":         string(status),
			"reason":     reason,
			"changed_by": actor,
		})
		if err != nil {
			return err
		}
		return insertControlEvent(ctx, tx, "tenant_lifecycle_update", tenantID, actor, payload)
	})
}

func (s *Store) SetEnvironmentLifecycleStatus(ctx context.Context, tenantSlug, envSlug string, status LifecycleStatus) error {
	return s.SetEnvironmentLifecycleStatusAudit(ctx, tenantSlug, envSlug, status, "", "")
}

func (s *Store) SetEnvironmentLifecycleStatusAudit(ctx context.Context, tenantSlug, envSlug string, status LifecycleStatus, actor, reason string) error {
	tenantSlug = strings.TrimSpace(tenantSlug)
	envSlug = strings.TrimSpace(envSlug)
	if tenantSlug == "" || envSlug == "" {
		return fmt.Errorf("tenant and environment are required")
	}
	actor, reason = sanitizeLifecycleAudit(actor, reason)
	if err := validateLifecycleTransitionAudit(status, reason); err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var (
			tenantID  string
			oldStatus string
			finalSlug string
		)
		finalSlug = envSlug
		if status == LifecycleStatusArchived {
			finalSlug = archivedLabelName(envSlug)
		}
		err := tx.QueryRow(ctx, `
WITH current_row AS (
	SELECT e.id, e.tenant_id, e.lifecycle_status
	FROM environments e
	JOIN tenants t ON t.id = e.tenant_id
	WHERE t.slug = $1
	  AND e.slug = $2
	FOR UPDATE
)
UPDATE environments e
SET lifecycle_status = $3,
    slug = $6,
    lifecycle_changed_at = now(),
    lifecycle_changed_by = $4,
    lifecycle_reason = $5,
    updated_at = now()
FROM current_row c
WHERE e.id = c.id
RETURNING c.tenant_id::text, c.lifecycle_status
`, tenantSlug, envSlug, string(status), actor, reason, finalSlug).Scan(&tenantID, &oldStatus)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrEnvironmentNotFound
			}
			return err
		}
		payload, err := json.Marshal(map[string]any{
			"tenant_slug": tenantSlug,
			"env_slug":    envSlug,
			"final_slug":  finalSlug,
			"from":        oldStatus,
			"to":          string(status),
			"reason":      reason,
			"changed_by":  actor,
		})
		if err != nil {
			return err
		}
		return insertControlEvent(ctx, tx, "environment_lifecycle_update", tenantID, actor, payload)
	})
}

func (s *Store) SetAPITokenLifecycleStatusAudit(ctx context.Context, tokenID string, status LifecycleStatus, actor, reason string) error {
	actor, reason = sanitizeLifecycleAudit(actor, reason)
	if err := validateLifecycleTransitionAudit(status, reason); err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var (
			tenantID  string
			tokenName string
			oldStatus string
		)
		err := tx.QueryRow(ctx, `
WITH current_row AS (
	SELECT id, tenant_id, name, lifecycle_status
	FROM api_tokens
	WHERE id = $1
	FOR UPDATE
)
UPDATE api_tokens tok
SET lifecycle_status = $2,
    lifecycle_changed_at = now(),
    lifecycle_changed_by = $3,
    lifecycle_reason = $4
FROM current_row c
WHERE tok.id = c.id
RETURNING c.tenant_id::text, c.name, c.lifecycle_status
`, tokenID, string(status), actor, reason).Scan(&tenantID, &tokenName, &oldStatus)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrAPITokenNotFound
			}
			return err
		}
		payload, err := json.Marshal(map[string]any{
			"token_id":   tokenID,
			"token_name": tokenName,
			"from":       oldStatus,
			"to":         string(status),
			"reason":     reason,
			"changed_by": actor,
		})
		if err != nil {
			return err
		}
		return insertControlEvent(ctx, tx, "api_token_lifecycle_update", tenantID, actor, payload)
	})
}

func (s *Store) DeleteEnvironmentAudit(ctx context.Context, tenantSlug, envSlug, actor, reason string) error {
	tenantSlug = strings.TrimSpace(tenantSlug)
	envSlug = strings.TrimSpace(envSlug)
	actor, reason = sanitizeLifecycleAudit(actor, reason)
	if tenantSlug == "" || envSlug == "" {
		return fmt.Errorf("tenant and environment are required")
	}
	if reason == "" {
		reason = "environment delete"
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var (
			tenantID         string
			envID            string
			workspaceID      string
			envPublicID      string
			databaseDSN      string
			oldStatus        string
			finalSlug        string
			activeStateCount int
		)
		if err := tx.QueryRow(ctx, `
SELECT t.id::text, e.id::text, t.workspace_id, e.env_public_id, COALESCE(e.database_dsn,''), e.lifecycle_status
FROM environments e
JOIN tenants t ON t.id = e.tenant_id
WHERE t.slug = $1
  AND e.slug = $2
FOR UPDATE
`, tenantSlug, envSlug).Scan(&tenantID, &envID, &workspaceID, &envPublicID, &databaseDSN, &oldStatus); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrEnvironmentNotFound
			}
			return err
		}
		if strings.TrimSpace(databaseDSN) != "" {
			return fmt.Errorf("environment has dedicated state storage; archive states manually before deleting the environment")
		}
		prefix := strings.TrimSpace(workspaceID) + "/" + strings.TrimSpace(envPublicID) + "/%"
		if err := tx.QueryRow(ctx, `
SELECT count(*)
FROM states
WHERE tenant_id = $1
  AND lifecycle_status = 'active'
  AND name LIKE $2
`, tenantID, prefix).Scan(&activeStateCount); err != nil {
			return err
		}
		if activeStateCount > 0 {
			return fmt.Errorf("environment still has %d active states", activeStateCount)
		}
		if _, err := tx.Exec(ctx, `
UPDATE api_tokens
SET lifecycle_status = 'archived',
    lifecycle_changed_at = now(),
    lifecycle_changed_by = $2,
    lifecycle_reason = $3
WHERE environment_id = $1
  AND lifecycle_status <> 'archived'
`, envID, actor, reason); err != nil {
			return err
		}
		finalSlug = archivedLabelName(envSlug)
		tag, err := tx.Exec(ctx, `
UPDATE environments
SET lifecycle_status = 'archived',
    slug = $2,
    lifecycle_changed_at = now(),
    lifecycle_changed_by = $3,
    lifecycle_reason = $4,
    updated_at = now()
WHERE id = $1
`, envID, finalSlug, actor, reason)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrEnvironmentNotFound
		}
		return insertControlEvent(ctx, tx, "environment_lifecycle_update", tenantID, actor, map[string]any{
			"tenant_slug": tenantSlug,
			"env_slug":    envSlug,
			"final_slug":  finalSlug,
			"from":        oldStatus,
			"to":          string(LifecycleStatusArchived),
			"reason":      reason,
			"changed_by":  actor,
		})
	})
}

func (s *Store) DeleteTenantAudit(ctx context.Context, slug, actor, reason string) error {
	slug = strings.TrimSpace(slug)
	actor, reason = sanitizeLifecycleAudit(actor, reason)
	if slug == "" {
		return fmt.Errorf("tenant slug is required")
	}
	if reason == "" {
		reason = "workspace delete"
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var (
			tenantID  string
			kind      string
			oldStatus string
		)
		if err := tx.QueryRow(ctx, `SELECT id::text, kind, lifecycle_status FROM tenants WHERE slug = $1 FOR UPDATE`, slug).Scan(&tenantID, &kind, &oldStatus); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrTenantNotFound
			}
			return err
		}
		if strings.EqualFold(strings.TrimSpace(kind), "personal") {
			return fmt.Errorf("personal workspace cannot be deleted")
		}
		var activeEnvironmentCount int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM environments WHERE tenant_id = $1 AND lifecycle_status = 'active'`, tenantID).Scan(&activeEnvironmentCount); err != nil {
			return err
		}
		if activeEnvironmentCount > 0 {
			return fmt.Errorf("workspace still has %d active environments", activeEnvironmentCount)
		}
		tag, err := tx.Exec(ctx, `
UPDATE tenants
SET lifecycle_status = 'archived',
    lifecycle_changed_at = now(),
    lifecycle_changed_by = $2,
    lifecycle_reason = $3,
    updated_at = now()
WHERE id = $1
`, tenantID, actor, reason)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrTenantNotFound
		}
		return insertControlEvent(ctx, tx, "tenant_lifecycle_update", tenantID, actor, map[string]any{
			"slug":       slug,
			"from":       oldStatus,
			"to":         string(LifecycleStatusArchived),
			"reason":     reason,
			"changed_by": actor,
		})
	})
}
