package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// EnvironmentTier is the isolation SKU for an environment.
type EnvironmentTier string

const (
	EnvironmentTierSharedHost    EnvironmentTier = "shared_host"
	EnvironmentTierDedicatedHost EnvironmentTier = "dedicated_host"
)

// EnvironmentStatus is provisioning lifecycle for an environment.
type EnvironmentStatus string

const (
	EnvironmentStatusProvisioning EnvironmentStatus = "provisioning"
	EnvironmentStatusReady        EnvironmentStatus = "ready"
	EnvironmentStatusFailed       EnvironmentStatus = "failed"
)

// EnvironmentRow is one row from environments.
type EnvironmentRow struct {
	ID                   string
	EnvPublicID          string
	TenantID             string
	WorkspaceID          string
	TenantSlug           string
	Slug                 string
	LifecycleStatus      LifecycleStatus
	Tier                 EnvironmentTier
	Status               EnvironmentStatus
	DatabaseInstanceKey  string
	DatabaseName         string
	DatabaseDSN          string
	HostConnectionName   string
	SourceDatabaseDSN    string
	ProvisionError       string
	ProvisionStartedAt   *time.Time
	ProvisionFinishedAt  *time.Time
	LastMigrationVersion int64
	LastMigrationAt      *time.Time
	LastMigrationError   string
	LifecycleChangedAt   *time.Time
	LifecycleChangedBy   string
	LifecycleReason      string
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// EnsureDefaultEnvironment creates the default environment for a tenant if missing.
func (s *Store) EnsureDefaultEnvironment(ctx context.Context, tenantID string) (EnvironmentRow, error) {
	rowID, err := ensureDefaultEnvironmentWithQuerier(ctx, s.pool, tenantID)
	if err != nil {
		return EnvironmentRow{}, err
	}
	return s.GetEnvironmentByID(ctx, rowID)
}

type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func ensureDefaultEnvironmentWithQuerier(ctx context.Context, q queryRower, tenantID string) (string, error) {
	var row EnvironmentRow
	err := q.QueryRow(ctx,
		`INSERT INTO environments (tenant_id, slug, tier, status)
		 VALUES ($1, 'default', 'shared_host', 'ready')
		 ON CONFLICT ON CONSTRAINT environments_tenant_slug_key DO UPDATE
		   SET updated_at = environments.updated_at
		 RETURNING id`,
		tenantID,
	).Scan(&row.ID)
	if err != nil {
		return "", err
	}
	return row.ID, nil
}

// CreateEnvironment inserts an environment for a tenant selected by
// workspace_id or slug.
func (s *Store) CreateEnvironment(ctx context.Context, tenantSlug, envSlug string, tier EnvironmentTier, instanceKey string) (EnvironmentRow, error) {
	tenantSlug = strings.TrimSpace(tenantSlug)
	envSlug = strings.TrimSpace(envSlug)
	if tenantSlug == "" || envSlug == "" {
		return EnvironmentRow{}, fmt.Errorf("workspace_id (or slug) and environment slug are required")
	}
	switch strings.TrimSpace(string(tier)) {
	case "", "shared":
		tier = EnvironmentTierSharedHost
	case "dedicated":
		tier = EnvironmentTierDedicatedHost
	}
	instanceKey = normalizeInstanceKey(instanceKey)
	tenant, err := s.GetTenantBySelector(ctx, tenantSlug)
	if err != nil {
		return EnvironmentRow{}, err
	}
	if tenant.LifecycleStatus != LifecycleStatusActive {
		return EnvironmentRow{}, fmt.Errorf("tenant %q is %s", tenant.Slug, tenant.LifecycleStatus)
	}
	if tenant.MaxEnvironments > 0 {
		var activeCount int
		if err := s.pool.QueryRow(ctx,
			`SELECT count(*)
			 FROM environments
			 WHERE tenant_id = $1
			   AND lifecycle_status = 'active'`,
			tenant.ID,
		).Scan(&activeCount); err != nil {
			return EnvironmentRow{}, err
		}
		if activeCount >= tenant.MaxEnvironments {
			return EnvironmentRow{}, fmt.Errorf("tenant %q environment limit reached (max_environments=%d)", tenant.Slug, tenant.MaxEnvironments)
		}
	}
	dbName := databaseNameFor(tenant.Slug, envSlug)
	status := string(EnvironmentStatusReady)
	if tier == EnvironmentTierDedicatedHost {
		status = string(EnvironmentStatusProvisioning)
	}
	var envID string
	err = s.pool.QueryRow(ctx,
		`INSERT INTO environments (tenant_id, slug, tier, status, database_instance_key, database_name, provision_started_at)
		 VALUES ($1, $2, $3, $4, $5, $6, CASE WHEN $3 = 'dedicated_host' THEN now() ELSE NULL END)
		 RETURNING id`,
		tenant.ID, envSlug, string(tier), status, instanceKey, dbName,
	).Scan(&envID)
	if err != nil {
		return EnvironmentRow{}, err
	}
	return s.GetEnvironmentByID(ctx, envID)
}

// RenameEnvironment changes the slug/nickname of an existing environment inside one workspace.
func (s *Store) RenameEnvironment(ctx context.Context, tenantSlug, currentSlug, newSlug, actor, reason string) (EnvironmentRow, error) {
	tenantSlug = strings.TrimSpace(tenantSlug)
	currentSlug = strings.TrimSpace(currentSlug)
	newSlug = strings.TrimSpace(newSlug)
	actor = strings.TrimSpace(actor)
	reason = strings.TrimSpace(reason)
	if tenantSlug == "" || currentSlug == "" || newSlug == "" {
		return EnvironmentRow{}, fmt.Errorf("tenant slug, current environment slug, and new environment slug are required")
	}
	if strings.EqualFold(currentSlug, newSlug) {
		return EnvironmentRow{}, fmt.Errorf("new environment nickname must differ from current slug %q", currentSlug)
	}
	tenant, err := s.GetTenantBySlug(ctx, tenantSlug)
	if err != nil {
		return EnvironmentRow{}, err
	}
	var envID string
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
UPDATE environments
SET slug = $3,
    updated_at = now(),
    lifecycle_changed_at = now(),
    lifecycle_changed_by = $4,
    lifecycle_reason = NULLIF($5, '')
WHERE tenant_id = $1
  AND slug = $2
RETURNING id::text`,
			tenant.ID, currentSlug, newSlug, actor, reason,
		).Scan(&envID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrEnvironmentNotFound
			}
			return err
		}
		return insertControlEvent(ctx, tx, "environment_renamed", tenant.ID, actor, map[string]any{
			"from_slug": currentSlug,
			"to_slug":   newSlug,
			"reason":    reason,
		})
	})
	if err != nil {
		return EnvironmentRow{}, err
	}
	return s.GetEnvironmentByID(ctx, envID)
}

// SetEnvironmentDSN updates the data-plane connection string for an environment.
func (s *Store) SetEnvironmentDSN(ctx context.Context, environmentID, dsn string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE environments
		 SET    database_dsn = NULLIF($2, ''),
		        status = 'ready',
		        updated_at = now()
		 WHERE  id = $1`,
		environmentID, strings.TrimSpace(dsn),
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrEnvironmentNotFound
	}
	return nil
}

// GetEnvironmentByID returns an environment row.
func (s *Store) GetEnvironmentByID(ctx context.Context, id string) (EnvironmentRow, error) {
	var row EnvironmentRow
	err := s.pool.QueryRow(ctx,
		`SELECT `+environmentSelectColumns()+environmentFromJoin()+`WHERE e.id = $1`,
		id,
	).Scan(environmentScanDest(&row)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return EnvironmentRow{}, ErrEnvironmentNotFound
	}
	if err != nil {
		return EnvironmentRow{}, err
	}
	return row, nil
}

// GetEnvironmentBySelector resolves an environment globally by internal id or env_public_id.
func (s *Store) GetEnvironmentBySelector(ctx context.Context, ref string) (EnvironmentRow, error) {
	var row EnvironmentRow
	err := s.pool.QueryRow(ctx,
		`SELECT `+environmentSelectColumns()+environmentFromJoin()+
			`WHERE e.id::text = $1 OR e.env_public_id = $1`,
		strings.TrimSpace(ref),
	).Scan(environmentScanDest(&row)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return EnvironmentRow{}, ErrEnvironmentNotFound
	}
	if err != nil {
		return EnvironmentRow{}, err
	}
	return row, nil
}

// GetEnvironmentByTenantSlug returns an environment for a tenant selector
// (workspace_id or slug) plus environment selector (env_public_id or slug).
func (s *Store) GetEnvironmentByTenantSlug(ctx context.Context, tenantSlug, envSlug string) (EnvironmentRow, error) {
	var row EnvironmentRow
	err := s.pool.QueryRow(ctx,
		`SELECT `+environmentSelectColumns()+environmentFromJoin()+
			`WHERE (t.slug = $1 OR t.workspace_id = $1)
			   AND (e.slug = $2 OR e.env_public_id = $2)`,
		strings.TrimSpace(tenantSlug), strings.TrimSpace(envSlug),
	).Scan(environmentScanDest(&row)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return EnvironmentRow{}, ErrEnvironmentNotFound
	}
	if err != nil {
		return EnvironmentRow{}, err
	}
	return row, nil
}

// ListEnvironments returns environments for one tenant selected by workspace_id or slug.
func (s *Store) ListEnvironments(ctx context.Context, tenantSlug string) ([]EnvironmentRow, error) {
	tenant, err := s.GetTenantBySelector(ctx, tenantSlug)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+environmentSelectColumns()+environmentFromJoin()+
			`WHERE e.tenant_id = $1
			   AND e.lifecycle_status = 'active'
			 ORDER BY e.slug`,
		tenant.ID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEnvironmentRows(rows)
}

func (s *Store) ListEnvironmentsAll(ctx context.Context, tenantSlug string) ([]EnvironmentRow, error) {
	tenant, err := s.GetTenantBySelector(ctx, tenantSlug)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+environmentSelectColumns()+environmentFromJoin()+
			`WHERE e.tenant_id = $1 ORDER BY e.slug`,
		tenant.ID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEnvironmentRows(rows)
}

func databaseNameFor(tenantSlug, envSlug string) string {
	s := strings.ToLower("kl_" + tenantSlug + "_" + envSlug)
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_':
			b.WriteByte('_')
		}
	}
	name := b.String()
	if name == "" {
		name = "env"
	}
	if len(name) > 48 {
		name = name[:48]
	}
	return name
}

func normalizeInstanceKey(key string) string {
	key = strings.ToLower(strings.TrimSpace(key))
	if key == "" {
		return "shared"
	}
	return key
}

// ListEnvironmentsWithDSN returns ready environments that have a data-plane DSN.
func (s *Store) ListEnvironmentsWithDSN(ctx context.Context) ([]EnvironmentRow, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+environmentSelectColumns()+environmentFromJoin()+
			`WHERE e.status = 'ready'
			   AND e.lifecycle_status = 'active'
			   AND t.lifecycle_status = 'active'
			   AND NULLIF(e.database_dsn, '') IS NOT NULL
			 ORDER BY t.slug, e.slug`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEnvironmentRows(rows)
}

var ErrEnvironmentNotFound = errors.New("environment not found")
