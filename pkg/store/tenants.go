package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// TenantRow is one row from tenants.
type TenantRow struct {
	ID                      string
	WorkspaceID             string
	Slug                    string
	Name                    string
	Kind                    string
	PersonalOwnerAccountID  string
	LifecycleStatus         LifecycleStatus
	LifecycleChangedAt      *time.Time
	LifecycleChangedBy      string
	LifecycleReason         string
	BillingPlan             string
	MaxEnvironments         int
	MaxStateResources       int
	MaxEnvironmentResources int
	StripeCustomerID        string
	StripeSubID             string
	StripeSubStatus         string
	StripePriceID           string
	StripePeriodEnd         *time.Time
}

const (
	StarterBillingPlan             = "starter"
	StarterMaxEnvironments         = 1
	StarterMaxStateResources       = 100
	StarterMaxEnvironmentResources = 500
)

// CreateTenant inserts a new tenant. slug must be unique (e.g. acme-corp).
func (s *Store) CreateTenant(ctx context.Context, slug, name string) (TenantRow, error) {
	return s.CreateTenantWithDefaultEnvironment(ctx, slug, name, true)
}

// CreateTenantWithDefaultEnvironment inserts a new tenant and optionally
// creates the default environment.
func (s *Store) CreateTenantWithDefaultEnvironment(ctx context.Context, slug, name string, createDefault bool) (TenantRow, error) {
	slug = strings.TrimSpace(slug)
	name = strings.TrimSpace(name)
	if slug == "" {
		generated, err := generateWorkspaceSlug("ws_")
		if err != nil {
			return TenantRow{}, err
		}
		slug = generated
	}
	if name == "" {
		name = slug
	}
	var row TenantRow
	for attempt := 0; attempt < 5; attempt++ {
		err := s.pool.QueryRow(ctx,
			`INSERT INTO tenants (slug, name) VALUES ($1, $2)
			 RETURNING id::text, workspace_id, slug, name, kind, COALESCE(personal_owner_account_id::text, ''), lifecycle_status,
			           lifecycle_changed_at, lifecycle_changed_by, lifecycle_reason,
			           billing_plan, max_environments, max_state_resources, max_environment_resources,
			           COALESCE(stripe_customer_id,''), COALESCE(stripe_subscription_id,''), COALESCE(stripe_subscription_status,''),
			           COALESCE(stripe_price_id,''), stripe_current_period_end`,
			slug, name,
		).Scan(
			&row.ID, &row.WorkspaceID, &row.Slug, &row.Name, &row.Kind, &row.PersonalOwnerAccountID, &row.LifecycleStatus,
			&row.LifecycleChangedAt, &row.LifecycleChangedBy, &row.LifecycleReason,
			&row.BillingPlan, &row.MaxEnvironments, &row.MaxStateResources, &row.MaxEnvironmentResources,
			&row.StripeCustomerID, &row.StripeSubID, &row.StripeSubStatus, &row.StripePriceID, &row.StripePeriodEnd,
		)
		if err == nil {
			break
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && strings.TrimSpace(pgErr.ConstraintName) == "tenants_slug_key" {
			generated, genErr := generateWorkspaceSlug("ws_")
			if genErr != nil {
				return TenantRow{}, genErr
			}
			slug = generated
			if name == row.Name || strings.TrimSpace(name) == "" {
				name = slug
			}
			continue
		}
		return TenantRow{}, err
	}
	if row.ID == "" {
		return TenantRow{}, fmt.Errorf("could not allocate workspace slug")
	}
	if createDefault {
		if _, err := s.EnsureDefaultEnvironment(ctx, row.ID); err != nil {
			return TenantRow{}, err
		}
	}
	return row, nil
}

// GetTenantBySlug returns a tenant or ErrTenantNotFound.
func (s *Store) GetTenantBySlug(ctx context.Context, slug string) (TenantRow, error) {
	var row TenantRow
	err := s.pool.QueryRow(ctx,
		`SELECT id::text, workspace_id, slug, name, kind, COALESCE(personal_owner_account_id::text, ''), lifecycle_status,
		        lifecycle_changed_at, lifecycle_changed_by, lifecycle_reason,
		        billing_plan, max_environments, max_state_resources, max_environment_resources,
		        COALESCE(stripe_customer_id,''), COALESCE(stripe_subscription_id,''), COALESCE(stripe_subscription_status,''),
		        COALESCE(stripe_price_id,''), stripe_current_period_end
		 FROM tenants WHERE slug = $1`,
		strings.TrimSpace(slug),
	).Scan(
		&row.ID, &row.WorkspaceID, &row.Slug, &row.Name, &row.Kind, &row.PersonalOwnerAccountID, &row.LifecycleStatus,
		&row.LifecycleChangedAt, &row.LifecycleChangedBy, &row.LifecycleReason,
		&row.BillingPlan, &row.MaxEnvironments, &row.MaxStateResources, &row.MaxEnvironmentResources,
		&row.StripeCustomerID, &row.StripeSubID, &row.StripeSubStatus, &row.StripePriceID, &row.StripePeriodEnd,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return TenantRow{}, ErrTenantNotFound
	}
	if err != nil {
		return TenantRow{}, err
	}
	return row, nil
}

func (s *Store) GetTenantByWorkspaceID(ctx context.Context, workspaceID string) (TenantRow, error) {
	var row TenantRow
	err := s.pool.QueryRow(ctx,
		`SELECT id::text, workspace_id, slug, name, kind, COALESCE(personal_owner_account_id::text, ''), lifecycle_status,
		        lifecycle_changed_at, lifecycle_changed_by, lifecycle_reason,
		        billing_plan, max_environments, max_state_resources, max_environment_resources,
		        COALESCE(stripe_customer_id,''), COALESCE(stripe_subscription_id,''), COALESCE(stripe_subscription_status,''),
		        COALESCE(stripe_price_id,''), stripe_current_period_end
		 FROM tenants WHERE workspace_id = $1`,
		strings.TrimSpace(workspaceID),
	).Scan(
		&row.ID, &row.WorkspaceID, &row.Slug, &row.Name, &row.Kind, &row.PersonalOwnerAccountID, &row.LifecycleStatus,
		&row.LifecycleChangedAt, &row.LifecycleChangedBy, &row.LifecycleReason,
		&row.BillingPlan, &row.MaxEnvironments, &row.MaxStateResources, &row.MaxEnvironmentResources,
		&row.StripeCustomerID, &row.StripeSubID, &row.StripeSubStatus, &row.StripePriceID, &row.StripePeriodEnd,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return TenantRow{}, ErrTenantNotFound
	}
	if err != nil {
		return TenantRow{}, err
	}
	return row, nil
}

func (s *Store) GetTenantBySelector(ctx context.Context, selector string) (TenantRow, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return TenantRow{}, ErrTenantNotFound
	}
	row, err := s.GetTenantByWorkspaceID(ctx, selector)
	if err == nil {
		return row, nil
	}
	if !errors.Is(err, ErrTenantNotFound) {
		return TenantRow{}, err
	}
	return s.GetTenantBySlug(ctx, selector)
}

// ListTenants returns all tenants ordered by slug.
func (s *Store) ListTenants(ctx context.Context) ([]TenantRow, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id::text, workspace_id, slug, name, kind, COALESCE(personal_owner_account_id::text, ''), lifecycle_status,
		        lifecycle_changed_at, lifecycle_changed_by, lifecycle_reason,
		        billing_plan, max_environments, max_state_resources, max_environment_resources,
		        COALESCE(stripe_customer_id,''), COALESCE(stripe_subscription_id,''), COALESCE(stripe_subscription_status,''),
		        COALESCE(stripe_price_id,''), stripe_current_period_end
		 FROM tenants
		 WHERE lifecycle_status = 'active'
		 ORDER BY slug`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TenantRow
	for rows.Next() {
		var r TenantRow
		if err := rows.Scan(
			&r.ID, &r.WorkspaceID, &r.Slug, &r.Name, &r.Kind, &r.PersonalOwnerAccountID, &r.LifecycleStatus,
			&r.LifecycleChangedAt, &r.LifecycleChangedBy, &r.LifecycleReason,
			&r.BillingPlan, &r.MaxEnvironments, &r.MaxStateResources, &r.MaxEnvironmentResources,
			&r.StripeCustomerID, &r.StripeSubID, &r.StripeSubStatus, &r.StripePriceID, &r.StripePeriodEnd,
		); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListTenantsAll returns all tenants, including suspended/archived.
func (s *Store) ListTenantsAll(ctx context.Context) ([]TenantRow, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id::text, workspace_id, slug, name, kind, COALESCE(personal_owner_account_id::text, ''), lifecycle_status,
		        lifecycle_changed_at, lifecycle_changed_by, lifecycle_reason,
		        billing_plan, max_environments, max_state_resources, max_environment_resources,
		        COALESCE(stripe_customer_id,''), COALESCE(stripe_subscription_id,''), COALESCE(stripe_subscription_status,''),
		        COALESCE(stripe_price_id,''), stripe_current_period_end
		 FROM tenants
		 ORDER BY slug`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TenantRow
	for rows.Next() {
		var r TenantRow
		if err := rows.Scan(
			&r.ID, &r.WorkspaceID, &r.Slug, &r.Name, &r.Kind, &r.PersonalOwnerAccountID, &r.LifecycleStatus,
			&r.LifecycleChangedAt, &r.LifecycleChangedBy, &r.LifecycleReason,
			&r.BillingPlan, &r.MaxEnvironments, &r.MaxStateResources, &r.MaxEnvironmentResources,
			&r.StripeCustomerID, &r.StripeSubID, &r.StripeSubStatus, &r.StripePriceID, &r.StripePeriodEnd,
		); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) SetTenantEntitlements(ctx context.Context, tenantSlug, billingPlan string, maxEnvironments, maxStateResources, maxEnvironmentResources int, actor, reason string) error {
	tenantSlug = strings.TrimSpace(tenantSlug)
	billingPlan = strings.TrimSpace(billingPlan)
	if tenantSlug == "" {
		return fmt.Errorf("tenant slug is required")
	}
	if maxEnvironments < 0 || maxStateResources < 0 || maxEnvironmentResources < 0 {
		return fmt.Errorf("entitlement limits must be >= 0")
	}
	if billingPlan == "" {
		billingPlan = "starter"
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var tenantID string
		if err := tx.QueryRow(ctx, `SELECT id::text FROM tenants WHERE slug = $1`, tenantSlug).Scan(&tenantID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrTenantNotFound
			}
			return err
		}
		_, err := tx.Exec(ctx,
			`UPDATE tenants
			 SET billing_plan = $2,
			     max_environments = $3,
			     max_state_resources = $4,
			     max_environment_resources = $5
			 WHERE slug = $1`,
			tenantSlug, billingPlan, maxEnvironments, maxStateResources, maxEnvironmentResources,
		)
		if err != nil {
			return err
		}
		return insertControlEvent(ctx, tx, "tenant_entitlements_update", tenantID, actor, map[string]any{
			"tenant_slug":               tenantSlug,
			"billing_plan":              billingPlan,
			"max_environments":          maxEnvironments,
			"max_state_resources":       maxStateResources,
			"max_environment_resources": maxEnvironmentResources,
			"reason":                    strings.TrimSpace(reason),
		})
	})
}

// ErrTenantNotFound is returned when a tenant slug/id does not exist.
var ErrTenantNotFound = errors.New("tenant not found")
