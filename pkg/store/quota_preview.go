package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/kilolockio/kilolock/pkg/auth"
)

// QuotaDimensionPreview describes one quota dimension's current and projected
// usage. SoftLimit/HardLimit equal 0 when the dimension is unlimited.
type QuotaDimensionPreview struct {
	SoftLimit     int  `json:"soft_limit"`
	HardLimit     int  `json:"hard_limit"`
	Current       int  `json:"current"`
	PlannedDelta  int  `json:"planned_delta"`
	Projected     int  `json:"projected"`
	RemainingSoft int  `json:"remaining_soft"`
	RemainingHard int  `json:"remaining_hard"`
	Unlimited     bool `json:"unlimited"`
	SoftExceeded  bool `json:"soft_exceeded"`
	HardExceeded  bool `json:"hard_exceeded"`
}

// QuotaPreview is the operator-facing snapshot of quota headroom for one
// state/environment pair under the caller's tenant.
type QuotaPreview struct {
	StateName   string                `json:"state_name"`
	TenantID    string                `json:"tenant_id"`
	TenantSlug  string                `json:"tenant_slug"`
	BillingPlan string                `json:"billing_plan"`
	State       QuotaDimensionPreview `json:"state"`
	Environment QuotaDimensionPreview `json:"environment"`
}

// PreviewStateQuota returns the current and projected resource counts for the
// named state. plannedDelta is the net managed-resource change the caller wants
// to evaluate, typically derived from a Terraform/KL plan as:
// create - delete - forget.
func (s *Store) PreviewStateQuota(ctx context.Context, stateName string, plannedDelta int) (*QuotaPreview, error) {
	p, ok := auth.FromContext(ctx)
	if !ok || strings.TrimSpace(p.TenantID) == "" {
		return nil, fmt.Errorf("PreviewStateQuota: tenant principal is required")
	}
	stateName = strings.Trim(strings.TrimSpace(stateName), "/")
	if stateName == "" {
		return nil, fmt.Errorf("PreviewStateQuota: state name is required")
	}

	var (
		tenantSlug               string
		billingPlan              string
		maxStateResources        int
		maxEnvironmentResources  int
		currentStateResources    int
		currentEnvironmentOthers int
	)
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`SELECT slug, billing_plan, max_state_resources, max_environment_resources
			 FROM tenants
			 WHERE id = $1`,
			p.TenantID,
		).Scan(&tenantSlug, &billingPlan, &maxStateResources, &maxEnvironmentResources); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrTenantNotFound
			}
			return err
		}

		if err := tx.QueryRow(ctx, `
SELECT COUNT(*)
FROM resources r
JOIN states s ON s.id = r.state_id
WHERE s.tenant_id = $1
  AND s.name = $2
  AND s.lifecycle_status = 'active'
  AND r.mode = 'managed'
  AND r.delete_serial IS NULL`,
			p.TenantID, stateName,
		).Scan(&currentStateResources); err != nil {
			return err
		}

		envPrefix := environmentStatePrefix(ctx, stateName)
		if envPrefix == "" {
			return nil
		}
		if err := tx.QueryRow(ctx, `
SELECT COUNT(*)
FROM resources r
JOIN states s ON s.id = r.state_id
WHERE s.tenant_id = $1
  AND s.lifecycle_status = 'active'
  AND r.mode = 'managed'
  AND r.delete_serial IS NULL
  AND s.name LIKE $2
  AND s.name <> $3`,
			p.TenantID, envPrefix+"%", stateName,
		).Scan(&currentEnvironmentOthers); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	stateCurrent := currentStateResources
	stateProjected := stateCurrent + plannedDelta
	if stateProjected < 0 {
		stateProjected = 0
	}
	envCurrent := currentEnvironmentOthers + currentStateResources
	envProjected := currentEnvironmentOthers + stateProjected

	return &QuotaPreview{
		StateName:   stateName,
		TenantID:    p.TenantID,
		TenantSlug:  tenantSlug,
		BillingPlan: billingPlan,
		State:       quotaDimensionPreview(maxStateResources, stateCurrent, plannedDelta, stateProjected),
		Environment: quotaDimensionPreview(maxEnvironmentResources, envCurrent, plannedDelta, envProjected),
	}, nil
}

func quotaDimensionPreview(softLimit, current, plannedDelta, projected int) QuotaDimensionPreview {
	if current < 0 {
		current = 0
	}
	if projected < 0 {
		projected = 0
	}
	hardLimit := hardQuotaFromSoft(softLimit)
	if softLimit <= 0 || hardLimit <= 0 {
		return QuotaDimensionPreview{
			SoftLimit:    0,
			HardLimit:    0,
			Current:      current,
			PlannedDelta: plannedDelta,
			Projected:    projected,
			Unlimited:    true,
		}
	}
	return QuotaDimensionPreview{
		SoftLimit:     softLimit,
		HardLimit:     hardLimit,
		Current:       current,
		PlannedDelta:  plannedDelta,
		Projected:     projected,
		RemainingSoft: softLimit - current,
		RemainingHard: hardLimit - current,
		SoftExceeded:  projected > softLimit,
		HardExceeded:  projected > hardLimit,
	}
}
