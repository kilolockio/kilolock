package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/kilolockio/kilolock/pkg/auth"
)

type EnvironmentStateLockDefaultMode string

const (
	EnvironmentStateLockDefaultAuto     EnvironmentStateLockDefaultMode = "auto"
	EnvironmentStateLockDefaultVanilla  EnvironmentStateLockDefaultMode = "vanilla"
	EnvironmentStateLockDefaultKilolock EnvironmentStateLockDefaultMode = "kilolock"
)

func (m EnvironmentStateLockDefaultMode) Valid() bool {
	return m == EnvironmentStateLockDefaultAuto || m == EnvironmentStateLockDefaultVanilla || m == EnvironmentStateLockDefaultKilolock
}

func normalizeEnvironmentStateLockDefaultMode(raw string) EnvironmentStateLockDefaultMode {
	mode := EnvironmentStateLockDefaultMode(strings.ToLower(strings.TrimSpace(raw)))
	switch mode {
	case "", EnvironmentStateLockDefaultAuto:
		return EnvironmentStateLockDefaultAuto
	case EnvironmentStateLockDefaultVanilla:
		return EnvironmentStateLockDefaultVanilla
	case EnvironmentStateLockDefaultKilolock:
		return EnvironmentStateLockDefaultKilolock
	default:
		return ""
	}
}

type initialStateCreator string

const (
	initialStateCreatorUnknown initialStateCreator = "unknown"
	initialStateCreatorBackend initialStateCreator = "backend"
	initialStateCreatorKL      initialStateCreator = "kl"
)

func initialStatePolicyForPrincipal(p auth.Principal, creator initialStateCreator) (bool, StateCoexistenceMode) {
	mode := normalizeEnvironmentStateLockDefaultMode(p.EnvironmentStateLockDefaultMode)
	switch mode {
	case EnvironmentStateLockDefaultVanilla:
		return true, StateCoexistenceStrict
	case EnvironmentStateLockDefaultKilolock:
		return false, StateCoexistenceWarn
	case EnvironmentStateLockDefaultAuto:
		switch creator {
		case initialStateCreatorBackend:
			return true, StateCoexistenceStrict
		case initialStateCreatorKL:
			return false, StateCoexistenceWarn
		default:
			return false, StateCoexistenceWarn
		}
	default:
		return false, StateCoexistenceWarn
	}
}

func (s *Store) SetEnvironmentStateLockDefaultMode(ctx context.Context, tenantSlug, envSlug string, mode EnvironmentStateLockDefaultMode) error {
	tenantSlug = strings.TrimSpace(tenantSlug)
	envSlug = strings.TrimSpace(envSlug)
	mode = normalizeEnvironmentStateLockDefaultMode(string(mode))
	if tenantSlug == "" || envSlug == "" {
		return fmt.Errorf("workspace_id (or slug) and environment slug are required")
	}
	if !mode.Valid() {
		return fmt.Errorf("invalid environment state lock default mode %q", mode)
	}
	tenant, err := s.GetTenantBySelector(ctx, tenantSlug)
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `
UPDATE environments
SET state_lock_default_mode = $3,
    updated_at = now()
WHERE tenant_id = $1
  AND slug = $2`,
		tenant.ID, envSlug, string(mode),
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrEnvironmentNotFound
	}
	return nil
}

func (s *Store) GetEnvironmentStateLockDefaultMode(ctx context.Context, tenantSlug, envSlug string) (EnvironmentStateLockDefaultMode, error) {
	tenantSlug = strings.TrimSpace(tenantSlug)
	envSlug = strings.TrimSpace(envSlug)
	if tenantSlug == "" || envSlug == "" {
		return "", errors.New("workspace_id (or slug) and environment slug are required")
	}
	tenant, err := s.GetTenantBySelector(ctx, tenantSlug)
	if err != nil {
		return "", err
	}
	var mode string
	err = s.pool.QueryRow(ctx, `
SELECT state_lock_default_mode
FROM environments
WHERE tenant_id = $1
  AND slug = $2`,
		tenant.ID, envSlug,
	).Scan(&mode)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrEnvironmentNotFound
	}
	if err != nil {
		return "", err
	}
	out := normalizeEnvironmentStateLockDefaultMode(mode)
	if !out.Valid() {
		return "", fmt.Errorf("environment %s/%s has invalid state_lock_default_mode %q", tenantSlug, envSlug, mode)
	}
	return out, nil
}
