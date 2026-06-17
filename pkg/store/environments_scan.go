package store

import "github.com/jackc/pgx/v5"

func environmentSelectColumns() string {
	return `e.id::text, e.env_public_id, e.tenant_id::text, t.workspace_id, t.slug, e.slug, e.lifecycle_status, e.tier, e.status,
	        COALESCE(e.database_instance_key, 'shared'),
	        COALESCE(e.database_name, ''), COALESCE(e.database_dsn, ''),
	        COALESCE(e.host_connection_name, ''),
	        COALESCE(e.source_database_dsn, ''),
	        COALESCE(e.provision_error, ''),
	        e.provision_started_at, e.provision_finished_at,
	        COALESCE(e.last_migration_version, 0),
	        e.last_migration_at,
	        COALESCE(e.last_migration_error, ''),
	        e.lifecycle_changed_at,
	        COALESCE(e.lifecycle_changed_by, ''),
	        COALESCE(e.lifecycle_reason, ''),
	        e.created_at, e.updated_at`
}

func environmentFromJoin() string {
	return ` FROM environments e JOIN tenants t ON t.id = e.tenant_id `
}

func environmentScanDest(row *EnvironmentRow) []any {
	return []any{
		&row.ID, &row.EnvPublicID, &row.TenantID, &row.WorkspaceID, &row.TenantSlug, &row.Slug, &row.LifecycleStatus, &row.Tier, &row.Status,
		&row.DatabaseInstanceKey,
		&row.DatabaseName, &row.DatabaseDSN, &row.HostConnectionName, &row.SourceDatabaseDSN,
		&row.ProvisionError, &row.ProvisionStartedAt, &row.ProvisionFinishedAt,
		&row.LastMigrationVersion, &row.LastMigrationAt, &row.LastMigrationError,
		&row.LifecycleChangedAt, &row.LifecycleChangedBy, &row.LifecycleReason,
		&row.CreatedAt, &row.UpdatedAt,
	}
}

func scanEnvironmentRows(rows pgx.Rows) ([]EnvironmentRow, error) {
	var out []EnvironmentRow
	for rows.Next() {
		var r EnvironmentRow
		if err := rows.Scan(environmentScanDest(&r)...); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
