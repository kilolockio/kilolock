package store

import (
	"context"
	"time"
)

// ListAllEnvironments returns every environment across tenants.
func (s *Store) ListAllEnvironments(ctx context.Context) ([]EnvironmentRow, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+environmentSelectColumns()+environmentFromJoin()+
			`ORDER BY t.slug, e.slug`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEnvironmentRows(rows)
}

func (s *Store) MarkEnvironmentMigrationError(ctx context.Context, environmentID, errMsg string) error {
	_, err := s.pool.Exec(ctx, `
UPDATE environments
SET last_migration_error = $2,
    last_migration_at = now(),
    updated_at = now()
WHERE id = $1
`, environmentID, errMsg)
	return err
}

func (s *Store) MarkEnvironmentMigrationSuccess(ctx context.Context, environmentID string, version int) error {
	_, err := s.pool.Exec(ctx, `
UPDATE environments
SET last_migration_version = $2,
    last_migration_error = '',
    last_migration_at = $3,
    updated_at = $3
WHERE id = $1
`, environmentID, version, time.Now().UTC())
	return err
}
