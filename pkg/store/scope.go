package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kilolockio/kilolock/pkg/auth"
)

// NewIsolated returns a Store for a dedicated environment database. State
// lookups filter by name only; tenant_id is still written on inserts from
// the authenticated principal.
func NewIsolated(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, isolated: true}
}

// stateByNameWhere builds "s.name = $1" or "s.name = $1 AND s.tenant_id = $2".
func (s *Store) stateByNameWhere(ctx context.Context, name string) (clause string, args []any) {
	if s.isolated {
		return "s.name = $1 AND s.lifecycle_status = 'active'", []any{name}
	}
	return "s.name = $1 AND s.tenant_id = $2 AND s.lifecycle_status = 'active'", []any{name, auth.TenantFromContext(ctx)}
}

// statesByNameWhere builds "name = $1" or "name = $1 AND tenant_id = $2" for states table.
func (s *Store) statesByNameWhere(ctx context.Context, name string) (clause string, args []any) {
	if s.isolated {
		return "name = $1 AND lifecycle_status = 'active'", []any{name}
	}
	return "name = $1 AND tenant_id = $2 AND lifecycle_status = 'active'", []any{name, auth.TenantFromContext(ctx)}
}

// statesTenantWhere returns a tenant filter for listing states, or empty when isolated.
func (s *Store) statesTenantWhere(ctx context.Context, param int, includeInactive bool) (clause string, arg any) {
	lifecycleFilter := ""
	if !includeInactive {
		lifecycleFilter = " AND s.lifecycle_status = 'active'"
	}
	if s.isolated {
		if lifecycleFilter == "" {
			return "", nil
		}
		return lifecycleFilter[5:], nil
	}
	return fmt.Sprintf("s.tenant_id = $%d%s", param, lifecycleFilter), auth.TenantFromContext(ctx)
}
