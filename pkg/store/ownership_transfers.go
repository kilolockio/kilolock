package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type OwnershipTransferProposal struct {
	ID                   string
	ResourceType         string
	ResourceID           string
	ResourceName         string
	TargetResourceName   string
	CurrentOwnerKind     string
	CurrentOwnerRef      string
	TargetOwnerKind      string
	TargetOwnerRef       string
	BillingImpact        bool
	Status               string
	InitiatedByAccountID string
	InitiatedBy          string
	InitiatedReason      string
	AcceptedByAccountID  string
	AcceptedBy           string
	AcceptedAt           *time.Time
	RejectedByAccountID  string
	RejectedBy           string
	RejectedAt           *time.Time
	CancelledByAccountID string
	CancelledBy          string
	CancelledAt          *time.Time
	ExpiresAt            *time.Time
	CreatedAt            *time.Time
	UpdatedAt            *time.Time
}

func scanOwnershipTransferProposal(row pgx.Row, item *OwnershipTransferProposal) error {
	return row.Scan(
		&item.ID, &item.ResourceType, &item.ResourceID, &item.ResourceName, &item.TargetResourceName,
		&item.CurrentOwnerKind, &item.CurrentOwnerRef,
		&item.TargetOwnerKind, &item.TargetOwnerRef,
		&item.BillingImpact, &item.Status,
		&item.InitiatedByAccountID, &item.InitiatedBy, &item.InitiatedReason,
		&item.AcceptedByAccountID, &item.AcceptedBy, &item.AcceptedAt,
		&item.RejectedByAccountID, &item.RejectedBy, &item.RejectedAt,
		&item.CancelledByAccountID, &item.CancelledBy, &item.CancelledAt,
		&item.ExpiresAt, &item.CreatedAt, &item.UpdatedAt,
	)
}

func scanOwnershipTransferRows(rows pgx.Rows) ([]OwnershipTransferProposal, error) {
	defer rows.Close()
	var out []OwnershipTransferProposal
	for rows.Next() {
		var item OwnershipTransferProposal
		if err := scanOwnershipTransferProposal(rows, &item); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) GetOwnershipTransferProposal(ctx context.Context, proposalID string) (OwnershipTransferProposal, error) {
	proposalID = strings.TrimSpace(proposalID)
	if proposalID == "" {
		return OwnershipTransferProposal{}, fmt.Errorf("proposal id is required")
	}
	var item OwnershipTransferProposal
	err := scanOwnershipTransferProposal(s.pool.QueryRow(ctx, `
SELECT
    id::text, resource_type, resource_id::text, resource_name, target_resource_name,
    current_owner_kind, current_owner_ref,
    target_owner_kind, target_owner_ref,
    billing_impact, status,
    COALESCE(initiated_by_account_id::text, ''), initiated_by, initiated_reason,
    COALESCE(accepted_by_account_id::text, ''), accepted_by, accepted_at,
    COALESCE(rejected_by_account_id::text, ''), rejected_by, rejected_at,
    COALESCE(cancelled_by_account_id::text, ''), cancelled_by, cancelled_at,
    expires_at, created_at, updated_at
FROM ownership_transfer_proposals
WHERE id = $1`, proposalID), &item)
	if errors.Is(err, pgx.ErrNoRows) {
		return OwnershipTransferProposal{}, fmt.Errorf("ownership transfer proposal not found")
	}
	if err != nil {
		return OwnershipTransferProposal{}, err
	}
	return item, nil
}

func (s *Store) ListOwnershipTransferProposalsByAccount(ctx context.Context, accountID string) ([]OwnershipTransferProposal, error) {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return nil, fmt.Errorf("account id is required")
	}
	rows, err := s.pool.Query(ctx, `
SELECT DISTINCT
    p.id::text, p.resource_type, p.resource_id::text, p.resource_name, p.target_resource_name,
    p.current_owner_kind, p.current_owner_ref,
    p.target_owner_kind, p.target_owner_ref,
    p.billing_impact, p.status,
    COALESCE(p.initiated_by_account_id::text, ''), p.initiated_by, p.initiated_reason,
    COALESCE(p.accepted_by_account_id::text, ''), p.accepted_by, p.accepted_at,
    COALESCE(p.rejected_by_account_id::text, ''), p.rejected_by, p.rejected_at,
    COALESCE(p.cancelled_by_account_id::text, ''), p.cancelled_by, p.cancelled_at,
    p.expires_at, p.created_at, p.updated_at
FROM ownership_transfer_proposals p
JOIN tenant_memberships tm
  ON tm.account_id = $1
 AND tm.revoked_at IS NULL
 AND (tm.tenant_slug = p.current_owner_ref OR tm.tenant_slug = p.target_owner_ref)
ORDER BY p.created_at DESC`, accountID)
	if err != nil {
		return nil, err
	}
	return scanOwnershipTransferRows(rows)
}

func (s *Store) ListOwnershipTransferProposals(ctx context.Context, tenantSlug, status string) ([]OwnershipTransferProposal, error) {
	tenantSlug = strings.TrimSpace(tenantSlug)
	status = strings.TrimSpace(status)
	rows, err := s.pool.Query(ctx, `
SELECT
    id::text, resource_type, resource_id::text, resource_name, target_resource_name,
    current_owner_kind, current_owner_ref,
    target_owner_kind, target_owner_ref,
    billing_impact, status,
    COALESCE(initiated_by_account_id::text, ''), initiated_by, initiated_reason,
    COALESCE(accepted_by_account_id::text, ''), accepted_by, accepted_at,
    COALESCE(rejected_by_account_id::text, ''), rejected_by, rejected_at,
    COALESCE(cancelled_by_account_id::text, ''), cancelled_by, cancelled_at,
    expires_at, created_at, updated_at
FROM ownership_transfer_proposals
WHERE ($1 = '' OR current_owner_ref = $1 OR target_owner_ref = $1)
  AND ($2 = '' OR status = $2)
ORDER BY created_at DESC`, tenantSlug, status)
	if err != nil {
		return nil, err
	}
	return scanOwnershipTransferRows(rows)
}

func (s *Store) CreateEnvironmentOwnershipTransferProposal(ctx context.Context, sourceTenantSlug, environmentSlug, targetTenantSlug, initiatedByAccountID, initiatedBy, reason string) (OwnershipTransferProposal, error) {
	return s.createEnvironmentOwnershipTransferProposal(ctx, sourceTenantSlug, environmentSlug, targetTenantSlug, initiatedByAccountID, initiatedBy, reason, false)
}

func (s *Store) CreateEnvironmentOwnershipTransferProposalByOperator(ctx context.Context, sourceTenantSlug, environmentSlug, targetTenantSlug, actor, reason string) (OwnershipTransferProposal, error) {
	return s.createEnvironmentOwnershipTransferProposal(ctx, sourceTenantSlug, environmentSlug, targetTenantSlug, "", actor, reason, true)
}

func (s *Store) createEnvironmentOwnershipTransferProposal(ctx context.Context, sourceTenantSlug, environmentSlug, targetTenantSlug, initiatedByAccountID, initiatedBy, reason string, skipMembershipCheck bool) (OwnershipTransferProposal, error) {
	sourceTenantSlug = strings.TrimSpace(sourceTenantSlug)
	environmentSlug = strings.TrimSpace(environmentSlug)
	targetTenantSlug = strings.TrimSpace(targetTenantSlug)
	initiatedByAccountID = strings.TrimSpace(initiatedByAccountID)
	initiatedBy = strings.TrimSpace(initiatedBy)
	reason = strings.TrimSpace(reason)
	if sourceTenantSlug == "" || environmentSlug == "" || targetTenantSlug == "" {
		return OwnershipTransferProposal{}, fmt.Errorf("source workspace, environment, and target workspace are required")
	}
	if !skipMembershipCheck && initiatedByAccountID == "" {
		return OwnershipTransferProposal{}, fmt.Errorf("initiator account is required")
	}
	if sourceTenantSlug == targetTenantSlug {
		return OwnershipTransferProposal{}, fmt.Errorf("target tenant must differ from current owner")
	}
	var out OwnershipTransferProposal
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		sourceTenant, err := s.GetTenantBySlug(ctx, sourceTenantSlug)
		if err != nil {
			return err
		}
		targetTenant, err := s.GetTenantBySelector(ctx, targetTenantSlug)
		if err != nil {
			return err
		}
		env, err := s.GetEnvironmentByTenantSlug(ctx, sourceTenantSlug, environmentSlug)
		if err != nil {
			return err
		}
		if env.Tier != EnvironmentTierSharedHost {
			return fmt.Errorf("ownership transfer is currently supported only for shared-host environments; migrate state manually for dedicated or BYODB environments")
		}
		if !skipMembershipCheck {
			if err := requireTenantRoleWithTx(ctx, tx, initiatedByAccountID, sourceTenantSlug, "owner"); err != nil {
				return err
			}
		}
		var existing int
		if err := tx.QueryRow(ctx, `
SELECT count(*)
FROM ownership_transfer_proposals
WHERE resource_type = 'environment'
  AND resource_id = $1
  AND status = 'pending'`, env.ID).Scan(&existing); err != nil {
			return err
		}
		if existing > 0 {
			return fmt.Errorf("environment %q already has a pending transfer proposal", environmentSlug)
		}
		targetEnvironmentSlug := env.Slug
		if err := scanOwnershipTransferProposal(tx.QueryRow(ctx, `
INSERT INTO ownership_transfer_proposals (
    resource_type, resource_id, resource_name, target_resource_name,
    current_owner_kind, current_owner_ref,
    target_owner_kind, target_owner_ref,
    billing_impact, status, initiated_by_account_id, initiated_by, initiated_reason
) VALUES (
    'environment', $1, $2, $3,
    'tenant', $4,
    'tenant', $5,
    true, 'pending', $6, $7, $8
)
RETURNING
    id::text, resource_type, resource_id::text, resource_name, target_resource_name,
    current_owner_kind, current_owner_ref,
    target_owner_kind, target_owner_ref,
    billing_impact, status,
    COALESCE(initiated_by_account_id::text, ''), initiated_by, initiated_reason,
    COALESCE(accepted_by_account_id::text, ''), accepted_by, accepted_at,
    COALESCE(rejected_by_account_id::text, ''), rejected_by, rejected_at,
    COALESCE(cancelled_by_account_id::text, ''), cancelled_by, cancelled_at,
    expires_at, created_at, updated_at
`, env.ID, env.Slug, targetEnvironmentSlug, sourceTenantSlug, targetTenant.Slug, portalNullIfEmpty(initiatedByAccountID), initiatedBy, reason), &out); err != nil {
			return err
		}
		_ = insertControlEvent(ctx, tx, "environment_transfer_proposed", sourceTenant.ID, initiatedBy, map[string]any{
			"environment_slug":        env.Slug,
			"target_environment_slug": targetEnvironmentSlug,
			"target_tenant":           targetTenant.Slug,
			"reason":                  reason,
		})
		_ = insertControlEvent(ctx, tx, "environment_transfer_pending_inbound", targetTenant.ID, initiatedBy, map[string]any{
			"environment_slug":        env.Slug,
			"target_environment_slug": targetEnvironmentSlug,
			"source_tenant":           sourceTenantSlug,
			"reason":                  reason,
		})
		return nil
	})
	return out, err
}

func requireTenantRoleWithTx(ctx context.Context, tx pgx.Tx, accountID, tenantSlug string, allowedRoles ...string) error {
	roleSet := make(map[string]struct{}, len(allowedRoles))
	for _, role := range allowedRoles {
		roleSet[strings.ToLower(strings.TrimSpace(role))] = struct{}{}
	}
	rows, err := tx.Query(ctx, `
SELECT role
FROM tenant_memberships
WHERE account_id = $1
  AND tenant_slug = $2
  AND revoked_at IS NULL`, accountID, tenantSlug)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var role string
		if err := rows.Scan(&role); err != nil {
			return err
		}
		if _, ok := roleSet[strings.ToLower(strings.TrimSpace(role))]; ok {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return fmt.Errorf("required role missing for tenant %q", tenantSlug)
}

func (s *Store) AcceptOwnershipTransferProposal(ctx context.Context, proposalID, accountID, actor string) (OwnershipTransferProposal, error) {
	return s.resolveOwnershipTransferProposal(ctx, proposalID, accountID, actor, "", "accept")
}

func (s *Store) AcceptOwnershipTransferProposalWithTarget(ctx context.Context, proposalID, accountID, actor, targetResourceName string) (OwnershipTransferProposal, error) {
	return s.resolveOwnershipTransferProposal(ctx, proposalID, accountID, actor, targetResourceName, "accept")
}

func (s *Store) AcceptOwnershipTransferProposalByOperator(ctx context.Context, proposalID, actor string) (OwnershipTransferProposal, error) {
	return s.resolveOwnershipTransferProposal(ctx, proposalID, "", actor, "", "accept_operator")
}

func (s *Store) AcceptOwnershipTransferProposalByOperatorWithTarget(ctx context.Context, proposalID, actor, targetResourceName string) (OwnershipTransferProposal, error) {
	return s.resolveOwnershipTransferProposal(ctx, proposalID, "", actor, targetResourceName, "accept_operator")
}

func (s *Store) RejectOwnershipTransferProposal(ctx context.Context, proposalID, accountID, actor string) (OwnershipTransferProposal, error) {
	return s.resolveOwnershipTransferProposal(ctx, proposalID, accountID, actor, "", "reject")
}

func (s *Store) RejectOwnershipTransferProposalByOperator(ctx context.Context, proposalID, actor string) (OwnershipTransferProposal, error) {
	return s.resolveOwnershipTransferProposal(ctx, proposalID, "", actor, "", "reject_operator")
}

func (s *Store) CancelOwnershipTransferProposal(ctx context.Context, proposalID, accountID, actor string) (OwnershipTransferProposal, error) {
	return s.resolveOwnershipTransferProposal(ctx, proposalID, accountID, actor, "", "cancel")
}

func (s *Store) CancelOwnershipTransferProposalByOperator(ctx context.Context, proposalID, actor string) (OwnershipTransferProposal, error) {
	return s.resolveOwnershipTransferProposal(ctx, proposalID, "", actor, "", "cancel_operator")
}

func (s *Store) resolveOwnershipTransferProposal(ctx context.Context, proposalID, accountID, actor, targetResourceName, action string) (OwnershipTransferProposal, error) {
	proposalID = strings.TrimSpace(proposalID)
	accountID = strings.TrimSpace(accountID)
	actor = strings.TrimSpace(actor)
	targetResourceName = strings.TrimSpace(targetResourceName)
	if proposalID == "" {
		return OwnershipTransferProposal{}, fmt.Errorf("proposal id is required")
	}
	var out OwnershipTransferProposal
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var sourceTenantID, targetTenantID string
		err := tx.QueryRow(ctx, `
SELECT
    p.id::text, p.resource_type, p.resource_id::text, p.resource_name, p.target_resource_name,
    p.current_owner_kind, p.current_owner_ref,
    p.target_owner_kind, p.target_owner_ref,
    p.billing_impact, p.status,
    COALESCE(p.initiated_by_account_id::text, ''), p.initiated_by, p.initiated_reason,
    COALESCE(p.accepted_by_account_id::text, ''), p.accepted_by, p.accepted_at,
    COALESCE(p.rejected_by_account_id::text, ''), p.rejected_by, p.rejected_at,
    COALESCE(p.cancelled_by_account_id::text, ''), p.cancelled_by, p.cancelled_at,
    p.expires_at, p.created_at, p.updated_at,
    ts.id::text, tt.id::text
FROM ownership_transfer_proposals p
JOIN tenants ts ON ts.slug = p.current_owner_ref
JOIN tenants tt ON tt.slug = p.target_owner_ref
WHERE p.id = $1
FOR UPDATE`, proposalID).Scan(
			&out.ID, &out.ResourceType, &out.ResourceID, &out.ResourceName, &out.TargetResourceName,
			&out.CurrentOwnerKind, &out.CurrentOwnerRef,
			&out.TargetOwnerKind, &out.TargetOwnerRef,
			&out.BillingImpact, &out.Status,
			&out.InitiatedByAccountID, &out.InitiatedBy, &out.InitiatedReason,
			&out.AcceptedByAccountID, &out.AcceptedBy, &out.AcceptedAt,
			&out.RejectedByAccountID, &out.RejectedBy, &out.RejectedAt,
			&out.CancelledByAccountID, &out.CancelledBy, &out.CancelledAt,
			&out.ExpiresAt, &out.CreatedAt, &out.UpdatedAt,
			&sourceTenantID, &targetTenantID,
		)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("ownership transfer proposal not found")
		}
		if err != nil {
			return err
		}
		if out.Status != "pending" {
			return fmt.Errorf("proposal is %s", out.Status)
		}
		switch action {
		case "accept":
			if err := requireTenantRoleWithTx(ctx, tx, accountID, out.TargetOwnerRef, "owner"); err != nil {
				return err
			}
			fallthrough
		case "accept_operator":
			if targetResourceName == "" {
				targetResourceName = out.TargetResourceName
			}
			if targetResourceName == "" {
				targetResourceName = out.ResourceName
			}
			var targetTenantIDRaw string
			var targetMaxEnvironments int
			if err := tx.QueryRow(ctx, `SELECT id::text, max_environments FROM tenants WHERE slug = $1`, out.TargetOwnerRef).Scan(&targetTenantIDRaw, &targetMaxEnvironments); err != nil {
				return err
			}
			var currentSlug string
			if err := tx.QueryRow(ctx, `SELECT slug FROM environments WHERE id = $1`, out.ResourceID).Scan(&currentSlug); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return ErrEnvironmentNotFound
				}
				return err
			}
			if targetMaxEnvironments > 0 {
				var activeCount int
				if err := tx.QueryRow(ctx, `
SELECT count(*)
FROM environments
WHERE tenant_id = $1
  AND lifecycle_status = 'active'`, targetTenantIDRaw).Scan(&activeCount); err != nil {
					return err
				}
				if activeCount >= targetMaxEnvironments {
					return fmt.Errorf("tenant %q environment limit reached (max_environments=%d)", out.TargetOwnerRef, targetMaxEnvironments)
				}
			}
			var collidingCount int
			if err := tx.QueryRow(ctx, `
SELECT count(*)
FROM environments
WHERE tenant_id = $1
  AND slug = $2
  AND lifecycle_status = 'active'`, targetTenantIDRaw, targetResourceName).Scan(&collidingCount); err != nil {
				return err
			}
			if collidingCount > 0 {
				return fmt.Errorf("target workspace already has an environment named %q; choose a different label during acceptance", targetResourceName)
			}
			_, err = tx.Exec(ctx, `
UPDATE environments
SET tenant_id = $2,
    slug = $3,
    updated_at = now()
WHERE id = $1`, out.ResourceID, targetTenantIDRaw, targetResourceName)
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `
UPDATE api_tokens
SET tenant_id = $2
WHERE environment_id = $1`, out.ResourceID, targetTenantIDRaw)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
UPDATE ownership_transfer_proposals
SET status = 'accepted',
    target_resource_name = $4,
    accepted_by_account_id = $2,
    accepted_by = $3,
    accepted_at = now(),
    updated_at = now()
WHERE id = $1`, proposalID, portalNullIfEmpty(accountID), actor, targetResourceName); err != nil {
				return err
			}
			_ = insertControlEvent(ctx, tx, "environment_transfer_accepted", sourceTenantID, actor, map[string]any{
				"environment_slug":        currentSlug,
				"target_environment_slug": targetResourceName,
				"target_tenant":           out.TargetOwnerRef,
			})
			_ = insertControlEvent(ctx, tx, "environment_transfer_received", targetTenantID, actor, map[string]any{
				"environment_slug":        currentSlug,
				"target_environment_slug": targetResourceName,
				"source_tenant":           out.CurrentOwnerRef,
			})
		case "reject":
			if err := requireTenantRoleWithTx(ctx, tx, accountID, out.TargetOwnerRef, "owner"); err != nil {
				return err
			}
			fallthrough
		case "reject_operator":
			if _, err := tx.Exec(ctx, `
UPDATE ownership_transfer_proposals
SET status = 'rejected',
    rejected_by_account_id = $2,
    rejected_by = $3,
    rejected_at = now(),
    updated_at = now()
WHERE id = $1`, proposalID, portalNullIfEmpty(accountID), actor); err != nil {
				return err
			}
			_ = insertControlEvent(ctx, tx, "environment_transfer_rejected", sourceTenantID, actor, map[string]any{
				"resource_name": out.ResourceName,
				"target_tenant": out.TargetOwnerRef,
			})
		case "cancel":
			if err := requireTenantRoleWithTx(ctx, tx, accountID, out.CurrentOwnerRef, "owner"); err != nil {
				return err
			}
			fallthrough
		case "cancel_operator":
			if _, err := tx.Exec(ctx, `
UPDATE ownership_transfer_proposals
SET status = 'cancelled',
    cancelled_by_account_id = $2,
    cancelled_by = $3,
    cancelled_at = now(),
    updated_at = now()
WHERE id = $1`, proposalID, portalNullIfEmpty(accountID), actor); err != nil {
				return err
			}
			_ = insertControlEvent(ctx, tx, "environment_transfer_cancelled", sourceTenantID, actor, map[string]any{
				"resource_name": out.ResourceName,
				"target_tenant": out.TargetOwnerRef,
			})
		default:
			return fmt.Errorf("unsupported action %q", action)
		}
		return tx.QueryRow(ctx, `
SELECT
    id::text, resource_type, resource_id::text, resource_name, target_resource_name,
    current_owner_kind, current_owner_ref,
    target_owner_kind, target_owner_ref,
    billing_impact, status,
    COALESCE(initiated_by_account_id::text, ''), initiated_by, initiated_reason,
    COALESCE(accepted_by_account_id::text, ''), accepted_by, accepted_at,
    COALESCE(rejected_by_account_id::text, ''), rejected_by, rejected_at,
    COALESCE(cancelled_by_account_id::text, ''), cancelled_by, cancelled_at,
    expires_at, created_at, updated_at
FROM ownership_transfer_proposals
WHERE id = $1`, proposalID).Scan(
			&out.ID, &out.ResourceType, &out.ResourceID, &out.ResourceName, &out.TargetResourceName,
			&out.CurrentOwnerKind, &out.CurrentOwnerRef,
			&out.TargetOwnerKind, &out.TargetOwnerRef,
			&out.BillingImpact, &out.Status,
			&out.InitiatedByAccountID, &out.InitiatedBy, &out.InitiatedReason,
			&out.AcceptedByAccountID, &out.AcceptedBy, &out.AcceptedAt,
			&out.RejectedByAccountID, &out.RejectedBy, &out.RejectedAt,
			&out.CancelledByAccountID, &out.CancelledBy, &out.CancelledAt,
			&out.ExpiresAt, &out.CreatedAt, &out.UpdatedAt,
		)
	})
	return out, err
}
