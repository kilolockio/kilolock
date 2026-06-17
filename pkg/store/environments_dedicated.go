package store

import (
	"context"
	"fmt"
	"strings"
)

// RequestDedicatedUpgrade moves an environment to dedicated_host tier and marks
// it provisioning. The current database_dsn is stored for cutover.
func (s *Store) RequestDedicatedUpgrade(ctx context.Context, tenantSlug, envSlug string) (EnvironmentRow, error) {
	env, err := s.GetEnvironmentByTenantSlug(ctx, tenantSlug, envSlug)
	if err != nil {
		return EnvironmentRow{}, err
	}
	if env.LifecycleStatus != LifecycleStatusActive {
		return EnvironmentRow{}, fmt.Errorf("environment %q/%q is %s", env.TenantSlug, env.Slug, env.LifecycleStatus)
	}
	if env.Tier == EnvironmentTierDedicatedHost && env.Status == EnvironmentStatusProvisioning {
		return env, nil
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE environments
		 SET    tier = 'dedicated_host',
		        status = 'provisioning',
		        source_database_dsn = NULLIF($2, ''),
		        provision_error = NULL,
		        provision_started_at = COALESCE(provision_started_at, now()),
		        provision_finished_at = NULL,
		        updated_at = now()
		 WHERE  id = $1`,
		env.ID, strings.TrimSpace(env.DatabaseDSN),
	)
	if err != nil {
		return EnvironmentRow{}, err
	}
	if tag.RowsAffected() == 0 {
		return EnvironmentRow{}, ErrEnvironmentNotFound
	}
	return s.GetEnvironmentByID(ctx, env.ID)
}

// ListDedicatedProvisioning returns environments awaiting dedicated provision.
func (s *Store) ListDedicatedProvisioning(ctx context.Context) ([]EnvironmentRow, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+environmentSelectColumns()+environmentFromJoin()+
			`WHERE e.tier = 'dedicated_host'
			   AND e.status = 'provisioning'
			   AND e.lifecycle_status = 'active'
			   AND t.lifecycle_status = 'active'
			 ORDER BY e.provision_started_at NULLS FIRST, t.slug, e.slug`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEnvironmentRows(rows)
}

// CompleteDedicatedProvision records a successful dedicated host cutover.
func (s *Store) CompleteDedicatedProvision(
	ctx context.Context,
	environmentID, hostConnectionName, databaseDSN string,
) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE environments
		 SET    host_connection_name = $2,
		        database_instance_key = CASE
		                                  WHEN NULLIF($2, '') IS NOT NULL THEN $2
		                                  ELSE database_instance_key
		                                END,
		        database_dsn = $3,
		        source_database_dsn = NULL,
		        status = 'ready',
		        provision_error = NULL,
		        provision_finished_at = now(),
		        updated_at = now()
		 WHERE  id = $1`,
		environmentID, hostConnectionName, databaseDSN,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrEnvironmentNotFound
	}
	return nil
}

// FailDedicatedProvision marks provisioning failed with an error message.
func (s *Store) FailDedicatedProvision(ctx context.Context, environmentID, errMsg string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE environments
		 SET    status = 'failed',
		        provision_error = $2,
		        provision_finished_at = now(),
		        updated_at = now()
		 WHERE  id = $1`,
		environmentID, errMsg,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrEnvironmentNotFound
	}
	return nil
}
