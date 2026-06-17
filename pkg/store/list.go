package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// StateInfo is the summary row returned by ListStates.
type StateInfo struct {
	Name             string
	Lineage          string
	Serial           int64
	TerraformVersion string
	ResourceCount    int
	UpdatedAt        string
	LifecycleStatus  LifecycleStatus
	Locked           bool
	ExclusiveLocks   bool
	CoexistenceMode  StateCoexistenceMode
}

// ListStates returns one StateInfo row per state, ordered by name.
// Scoped to the caller's tenant; cross-tenant lists are impossible
// by construction.
func (s *Store) ListStates(ctx context.Context) ([]StateInfo, error) {
	return s.listStatesWithLifecycle(ctx, false)
}

// ListStatesAll returns one StateInfo row per state, including archived/suspended.
func (s *Store) ListStatesAll(ctx context.Context) ([]StateInfo, error) {
	return s.listStatesWithLifecycle(ctx, true)
}

func (s *Store) listStatesWithLifecycle(ctx context.Context, includeInactive bool) ([]StateInfo, error) {
	tenantClause, tenantArg := s.statesTenantWhere(ctx, 1, includeInactive)
	q := `
		SELECT
		    s.name,
		    COALESCE(s.lineage::text, '')                              AS lineage,
		    COALESCE(sv.serial, 0)                                     AS serial,
		    COALESCE(sv.terraform_version, '')                         AS tf_version,
		    COALESCE((SELECT COUNT(*) FROM resources r
		              WHERE r.state_id = s.id AND r.mode = 'managed' AND r.delete_serial IS NULL), 0) AS resource_count,
		    to_char(s.updated_at AT TIME ZONE 'UTC',
		            'YYYY-MM-DD"T"HH24:MI:SS"Z"')                      AS updated_at,
		    s.lifecycle_status,
		    EXISTS (SELECT 1 FROM state_locks l WHERE l.state_id = s.id) AS locked,
		    s.exclusive_locks,
		    s.coexistence_mode
		FROM   states s
		LEFT   JOIN state_versions sv ON sv.id = s.current_version_id`
	if tenantClause != "" {
		q += `
		WHERE  ` + tenantClause
	}
	q += `
		ORDER  BY s.name`
	var (
		rows pgx.Rows
		err  error
	)
	if tenantClause != "" && !s.isolated {
		rows, err = s.pool.Query(ctx, q, tenantArg)
	} else {
		rows, err = s.pool.Query(ctx, q)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []StateInfo
	for rows.Next() {
		var si StateInfo
		if err := rows.Scan(&si.Name, &si.Lineage, &si.Serial, &si.TerraformVersion,
			&si.ResourceCount, &si.UpdatedAt, &si.LifecycleStatus, &si.Locked, &si.ExclusiveLocks, &si.CoexistenceMode); err != nil {
			return nil, err
		}
		out = append(out, si)
	}
	return out, rows.Err()
}

// CurrentSerial returns the serial of the currently-active state version
// for the named state, or ErrStateNotFound when the state does not exist
// (or has never been written).
func (s *Store) CurrentSerial(ctx context.Context, name string) (int64, error) {
	where, args := s.stateByNameWhere(ctx, name)
	q := `
		SELECT sv.serial
		FROM   states s
		JOIN   state_versions sv ON sv.id = s.current_version_id
		WHERE  ` + where
	var serial int64
	err := s.pool.QueryRow(ctx, q, args...).Scan(&serial)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrStateNotFound
	}
	return serial, err
}
