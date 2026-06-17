package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kilolockio/kilolock/pkg/auth"
)

// APITokenRow is metadata for a stored API token (never includes secret).
type APITokenRow struct {
	ID                 string
	TenantID           string
	TenantSlug         string
	EnvironmentID      string
	EnvSlug            string
	Name               string
	TokenPrefix        string
	LifecycleStatus    LifecycleStatus
	CreatedAt          time.Time
	RevokedAt          *time.Time
	LastUsedAt         *time.Time
	LifecycleChangedAt *time.Time
	LifecycleChangedBy string
	LifecycleReason    string
}

// CreateAPIToken inserts a new token for the tenant environment and returns
// the one-time plaintext secret. The tenant selector may be workspace_id or slug,
// and the environment selector may be env_public_id or slug. envSlug defaults to "default".
func (s *Store) CreateAPIToken(ctx context.Context, tenantSlug, envSlug, name string) (row APITokenRow, plaintext string, err error) {
	tenantSlug = strings.TrimSpace(tenantSlug)
	envSlug = strings.TrimSpace(envSlug)
	if envSlug == "" {
		envSlug = "default"
	}
	name = strings.TrimSpace(name)
	if tenantSlug == "" || name == "" {
		return APITokenRow{}, "", fmt.Errorf("workspace_id (or slug) and token name are required")
	}
	tenant, err := s.GetTenantBySelector(ctx, tenantSlug)
	if err != nil {
		return APITokenRow{}, "", err
	}
	if tenant.LifecycleStatus != LifecycleStatusActive {
		return APITokenRow{}, "", fmt.Errorf("tenant %q is %s", tenant.Slug, tenant.LifecycleStatus)
	}
	env, err := s.GetEnvironmentByTenantSlug(ctx, tenantSlug, envSlug)
	if err != nil {
		// Backward-compat fallback: only auto-create default env when the
		// requested environment is literally "default".
		if envSlug == "default" {
			if _, err2 := s.EnsureDefaultEnvironment(ctx, tenant.ID); err2 != nil {
				return APITokenRow{}, "", err2
			}
			env, err = s.GetEnvironmentByTenantSlug(ctx, tenantSlug, envSlug)
			if err != nil {
				return APITokenRow{}, "", err
			}
		} else {
			return APITokenRow{}, "", err
		}
	}
	if env.LifecycleStatus != LifecycleStatusActive {
		return APITokenRow{}, "", fmt.Errorf("environment %q/%q is %s", tenant.Slug, env.Slug, env.LifecycleStatus)
	}
	plaintext, hash, prefix, err := auth.NewAPIToken()
	if err != nil {
		return APITokenRow{}, "", err
	}
	err = s.pool.QueryRow(ctx,
		`INSERT INTO api_tokens (tenant_id, environment_id, name, token_hash, token_prefix)
		 VALUES ($1, $2, $3, $4, $5)
		 RETURNING id::text, lifecycle_status, created_at,
		           lifecycle_changed_at, lifecycle_changed_by, lifecycle_reason`,
		tenant.ID, env.ID, name, hash, prefix,
	).Scan(&row.ID, &row.LifecycleStatus, &row.CreatedAt,
		&row.LifecycleChangedAt, &row.LifecycleChangedBy, &row.LifecycleReason)
	if err != nil {
		return APITokenRow{}, "", err
	}
	row.TenantID = tenant.ID
	row.TenantSlug = tenant.Slug
	row.EnvironmentID = env.ID
	row.EnvSlug = env.Slug
	row.Name = name
	row.TokenPrefix = prefix
	return row, plaintext, nil
}

// AuthenticateAPIToken validates a presented secret. tenantSlug is
// required for HTTP Basic (username = slug); leave empty for Bearer.
func (s *Store) AuthenticateAPIToken(ctx context.Context, secret, tenantSlug string) (auth.Principal, error) {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return auth.Principal{}, auth.ErrUnauthenticated
	}
	hash := auth.HashAPIToken(secret)

	q := `
		SELECT t.id::text, t.workspace_id, t.slug, e.id::text, e.env_public_id, e.slug, COALESCE(e.database_instance_key, 'shared'), tok.name,
		       t.lifecycle_status, t.billing_plan, t.max_environments, t.max_state_resources, t.max_environment_resources
		FROM   api_tokens tok
		JOIN   tenants t ON t.id = tok.tenant_id
		JOIN   environments e ON e.id = tok.environment_id
		WHERE  tok.token_hash = $1
		  AND  tok.revoked_at IS NULL
		  AND  tok.lifecycle_status = 'active'
		  AND  t.lifecycle_status = 'active'
		  AND  e.lifecycle_status = 'active'
	`
	args := []any{hash}
	if tenantSlug != "" {
		q += ` AND (t.slug = $2 OR t.workspace_id = $2)`
		args = append(args, strings.TrimSpace(tenantSlug))
	}
	return s.scanPrincipal(ctx, q, args...)
}

func backendStateScope(stateName string) (workspaceID, environmentPublicID string, ok bool) {
	parts := strings.Split(strings.Trim(strings.TrimSpace(stateName), "/"), "/")
	if len(parts) < 2 {
		return "", "", false
	}
	workspaceID = strings.TrimSpace(parts[0])
	environmentPublicID = strings.TrimSpace(parts[1])
	if workspaceID == "" || environmentPublicID == "" {
		return "", "", false
	}
	return workspaceID, environmentPublicID, true
}

// AuthenticateBackendToken resolves either an environment automation token
// or a human portal PAT for Terraform HTTP backend requests.
func (s *Store) AuthenticateBackendToken(ctx context.Context, secret, tenantSlug, stateName string) (auth.Principal, error) {
	if p, err := s.AuthenticateAPIToken(ctx, secret, tenantSlug); err == nil {
		return p, nil
	} else if !errors.Is(err, auth.ErrUnauthenticated) {
		return auth.Principal{}, err
	}

	secret = strings.TrimSpace(secret)
	if secret == "" {
		return auth.Principal{}, auth.ErrUnauthenticated
	}
	workspaceID, environmentPublicID, ok := backendStateScope(stateName)
	if !ok {
		return auth.Principal{}, auth.ErrUnauthenticated
	}
	if tenantSlug = strings.TrimSpace(tenantSlug); tenantSlug != "" && tenantSlug != workspaceID {
		return auth.Principal{}, auth.ErrUnauthenticated
	}

	hash := auth.HashAPIToken(secret)
	var (
		tenantID, resolvedWorkspaceID, resolvedTenantSlug string
		envID, envPublicID, envSlug, instanceKey          string
		accountID, email                                  string
		tenantLifecycleStatus, billingPlan                string
		maxEnvironments, maxStateResources                int
		maxEnvironmentResources                           int
	)
	err := s.pool.QueryRow(ctx, `
SELECT t.id::text, t.workspace_id, t.slug, e.id::text, e.env_public_id, e.slug, COALESCE(e.database_instance_key, 'shared'),
       a.id::text, a.email, t.lifecycle_status, t.billing_plan, t.max_environments, t.max_state_resources, t.max_environment_resources
FROM portal_personal_access_tokens pat
JOIN portal_accounts a ON a.id = pat.account_id
JOIN tenant_memberships tm ON tm.account_id = a.id AND tm.revoked_at IS NULL
JOIN tenants t ON t.slug = tm.tenant_slug
JOIN environments e ON e.tenant_id = t.id
LEFT JOIN portal_environment_pat_grants g ON g.account_id = a.id AND g.environment_id = e.id AND g.revoked_at IS NULL
WHERE pat.token_hash = $1
  AND pat.revoked_at IS NULL
  AND t.workspace_id = $2
  AND e.env_public_id = $3
  AND t.lifecycle_status = 'active'
  AND e.lifecycle_status = 'active'
  AND (
        (t.kind = 'personal' AND t.personal_owner_account_id = a.id)
        OR g.id IS NOT NULL
      )
LIMIT 1`,
		hash, workspaceID, environmentPublicID,
	).Scan(
		&tenantID, &resolvedWorkspaceID, &resolvedTenantSlug, &envID, &envPublicID, &envSlug, &instanceKey,
		&accountID, &email, &tenantLifecycleStatus, &billingPlan, &maxEnvironments, &maxStateResources, &maxEnvironmentResources,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.Principal{}, auth.ErrUnauthenticated
	}
	if err != nil {
		return auth.Principal{}, err
	}
	_, _ = s.pool.Exec(ctx, `
UPDATE portal_personal_access_tokens
SET last_used_at = now()
WHERE token_hash = $1
  AND revoked_at IS NULL`, hash)
	return auth.Principal{
		TenantID:                tenantID,
		WorkspaceID:             resolvedWorkspaceID,
		EnvironmentID:           envID,
		TenantSlug:              resolvedTenantSlug,
		EnvironmentPublicID:     envPublicID,
		EnvironmentSlug:         envSlug,
		DatabaseInstanceKey:     instanceKey,
		TenantLifecycleStatus:   tenantLifecycleStatus,
		BillingPlan:             billingPlan,
		MaxEnvironments:         maxEnvironments,
		MaxStateResources:       maxStateResources,
		MaxEnvironmentResources: maxEnvironmentResources,
		UserID:                  accountID,
		Email:                   email,
		Source:                  "portal-pat",
	}, nil
}

func (s *Store) scanPrincipal(ctx context.Context, q string, args ...any) (auth.Principal, error) {
	var tenantID, workspaceID, slug, envID, envPublicID, envSlug, instanceKey, tokenName, tenantLifecycleStatus, billingPlan string
	var maxEnvironments, maxStateResources, maxEnvironmentResources int
	err := s.pool.QueryRow(ctx, q, args...).Scan(
		&tenantID, &workspaceID, &slug, &envID, &envPublicID, &envSlug, &instanceKey, &tokenName,
		&tenantLifecycleStatus, &billingPlan, &maxEnvironments, &maxStateResources, &maxEnvironmentResources,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.Principal{}, auth.ErrUnauthenticated
	}
	if err != nil {
		return auth.Principal{}, err
	}
	// Best-effort last_used_at; ignore errors.
	_, _ = s.pool.Exec(ctx,
		`UPDATE api_tokens SET last_used_at = now()
		 WHERE token_hash = $1 AND revoked_at IS NULL`,
		args[0],
	)
	return auth.Principal{
		TenantID:                tenantID,
		WorkspaceID:             workspaceID,
		EnvironmentID:           envID,
		TenantSlug:              slug,
		EnvironmentPublicID:     envPublicID,
		EnvironmentSlug:         envSlug,
		DatabaseInstanceKey:     instanceKey,
		TenantLifecycleStatus:   tenantLifecycleStatus,
		BillingPlan:             billingPlan,
		MaxEnvironments:         maxEnvironments,
		MaxStateResources:       maxStateResources,
		MaxEnvironmentResources: maxEnvironmentResources,
		Email:                   slug + "/" + envSlug + "/" + tokenName,
		Source:                  "http-api-token",
	}, nil
}

// ListAPITokens returns tokens for one tenant selected by workspace_id or slug (metadata only).
func (s *Store) ListAPITokens(ctx context.Context, tenantSlug string) ([]APITokenRow, error) {
	tenant, err := s.GetTenantBySelector(ctx, tenantSlug)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx,
		`SELECT tok.id::text, e.id::text, e.slug, tok.name, tok.token_prefix,
		        tok.lifecycle_status, tok.created_at, tok.revoked_at, tok.last_used_at,
		        tok.lifecycle_changed_at, tok.lifecycle_changed_by, tok.lifecycle_reason
		 FROM api_tokens tok
		 JOIN environments e ON e.id = tok.environment_id
		 WHERE tok.tenant_id = $1
		   AND tok.lifecycle_status = 'active'
		 ORDER BY tok.created_at DESC`,
		tenant.ID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APITokenRow
	for rows.Next() {
		var r APITokenRow
		r.TenantID = tenant.ID
		r.TenantSlug = tenant.Slug
		if err := rows.Scan(&r.ID, &r.EnvironmentID, &r.EnvSlug, &r.Name, &r.TokenPrefix,
			&r.LifecycleStatus, &r.CreatedAt, &r.RevokedAt, &r.LastUsedAt,
			&r.LifecycleChangedAt, &r.LifecycleChangedBy, &r.LifecycleReason); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) ListAPITokensAll(ctx context.Context, tenantSlug string) ([]APITokenRow, error) {
	tenant, err := s.GetTenantBySelector(ctx, tenantSlug)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx,
		`SELECT tok.id::text, e.id::text, e.slug, tok.name, tok.token_prefix,
		        tok.lifecycle_status, tok.created_at, tok.revoked_at, tok.last_used_at,
		        tok.lifecycle_changed_at, tok.lifecycle_changed_by, tok.lifecycle_reason
		 FROM api_tokens tok
		 JOIN environments e ON e.id = tok.environment_id
		 WHERE tok.tenant_id = $1
		 ORDER BY tok.created_at DESC`,
		tenant.ID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APITokenRow
	for rows.Next() {
		var r APITokenRow
		r.TenantID = tenant.ID
		r.TenantSlug = tenant.Slug
		if err := rows.Scan(&r.ID, &r.EnvironmentID, &r.EnvSlug, &r.Name, &r.TokenPrefix,
			&r.LifecycleStatus, &r.CreatedAt, &r.RevokedAt, &r.LastUsedAt,
			&r.LifecycleChangedAt, &r.LifecycleChangedBy, &r.LifecycleReason); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetAPITokenByID returns token metadata for a global token id.
func (s *Store) GetAPITokenByID(ctx context.Context, tokenID string) (APITokenRow, error) {
	tokenID = strings.TrimSpace(tokenID)
	if tokenID == "" {
		return APITokenRow{}, ErrAPITokenNotFound
	}
	var r APITokenRow
	err := s.pool.QueryRow(ctx,
		`SELECT tok.id::text, t.id::text, t.slug, e.id::text, e.slug, tok.name, tok.token_prefix,
		        tok.lifecycle_status, tok.created_at, tok.revoked_at, tok.last_used_at,
		        tok.lifecycle_changed_at, tok.lifecycle_changed_by, tok.lifecycle_reason
		 FROM api_tokens tok
		 JOIN tenants t ON t.id = tok.tenant_id
		 JOIN environments e ON e.id = tok.environment_id
		 WHERE tok.id = $1`,
		tokenID,
	).Scan(
		&r.ID, &r.TenantID, &r.TenantSlug, &r.EnvironmentID, &r.EnvSlug, &r.Name, &r.TokenPrefix,
		&r.LifecycleStatus, &r.CreatedAt, &r.RevokedAt, &r.LastUsedAt,
		&r.LifecycleChangedAt, &r.LifecycleChangedBy, &r.LifecycleReason,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return APITokenRow{}, ErrAPITokenNotFound
	}
	if err != nil {
		return APITokenRow{}, err
	}
	return r, nil
}

// RevokeAPIToken marks a token revoked by id (global id, admin use).
func (s *Store) RevokeAPIToken(ctx context.Context, tokenID string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE api_tokens SET revoked_at = now()
		 WHERE id = $1 AND revoked_at IS NULL`,
		tokenID,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrAPITokenNotFound
	}
	return nil
}

func (s *Store) DeleteAPITokenAudit(ctx context.Context, tokenID, actor, reason string) error {
	tokenID = strings.TrimSpace(tokenID)
	actor = strings.TrimSpace(actor)
	reason = strings.TrimSpace(reason)
	if tokenID == "" {
		return ErrAPITokenNotFound
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var (
			tenantID  string
			tokenName string
		)
		if err := tx.QueryRow(ctx, `
DELETE FROM api_tokens
WHERE id = $1
RETURNING tenant_id::text, name`, tokenID).Scan(&tenantID, &tokenName); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrAPITokenNotFound
			}
			return err
		}
		payload, err := json.Marshal(map[string]any{
			"token_id":   tokenID,
			"token_name": tokenName,
			"reason":     reason,
			"changed_by": actor,
		})
		if err != nil {
			return err
		}
		return insertControlEvent(ctx, tx, "api_token_deleted", tenantID, actor, payload)
	})
}

func (s *Store) SetAPITokenLifecycleStatus(ctx context.Context, tokenID string, status LifecycleStatus) error {
	return s.SetAPITokenLifecycleStatusAudit(ctx, tokenID, status, "", "")
}

// BootstrapTenantToken ensures a tenant and token exist (idempotent).
// Used by docker-compose / first-run setup.
func (s *Store) BootstrapTenantToken(ctx context.Context, slug, tenantName, tokenName, plaintext string) error {
	tenant, err := s.GetTenantBySlug(ctx, slug)
	if errors.Is(err, ErrTenantNotFound) {
		tenant, err = s.CreateTenant(ctx, slug, tenantName)
	}
	if err != nil {
		return err
	}
	env, err := s.EnsureDefaultEnvironment(ctx, tenant.ID)
	if err != nil {
		return err
	}
	hash := auth.HashAPIToken(plaintext)
	prefix := plaintext
	if len(prefix) > 12 {
		prefix = prefix[:12] + "…"
	}
	_, err = s.pool.Exec(ctx,
		`INSERT INTO api_tokens (tenant_id, environment_id, name, token_hash, token_prefix)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT ON CONSTRAINT api_tokens_environment_name_key DO UPDATE
		   SET token_hash = EXCLUDED.token_hash,
		       token_prefix = EXCLUDED.token_prefix,
		       revoked_at = NULL`,
		tenant.ID, env.ID, tokenName, hash, prefix,
	)
	return err
}

var ErrAPITokenNotFound = errors.New("api token not found")
