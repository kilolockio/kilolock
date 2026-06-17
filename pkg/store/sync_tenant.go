package store

import (
	"context"
)

// EnsureTenantOnDataPlane inserts the tenant row into a dedicated environment
// database so tenant_id filters match the control plane UUID.
func (s *Store) EnsureTenantOnDataPlane(ctx context.Context, id, slug, name, lifecycleStatus, billingPlan string, maxEnvironments, maxStateResources, maxEnvironmentResources int) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO tenants (id, slug, name, lifecycle_status, billing_plan, max_environments, max_state_resources, max_environment_resources)
		 VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8)
		 ON CONFLICT (id) DO UPDATE
		 SET slug = EXCLUDED.slug,
		     name = EXCLUDED.name,
		     lifecycle_status = EXCLUDED.lifecycle_status,
		     billing_plan = EXCLUDED.billing_plan,
		     max_environments = EXCLUDED.max_environments,
		     max_state_resources = EXCLUDED.max_state_resources,
		     max_environment_resources = EXCLUDED.max_environment_resources`,
		id, slug, name, lifecycleStatus, billingPlan, maxEnvironments, maxStateResources, maxEnvironmentResources,
	)
	return err
}
