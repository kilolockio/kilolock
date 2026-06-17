package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrConfigNotFound is returned by GetProviderConfig when no row
// exists for the (source, alias) pair. Callers should errors.Is on
// this rather than checking the message text.
var ErrConfigNotFound = errors.New("provider config not found")

// ProviderConfigEntry is one row from provider_configs, joined with
// its decoded Config map. The Config map mirrors the attribute names
// of the provider's configuration block: e.g. for the aws provider,
// keys like "region" and "profile" appear at the top level.
//
// Config is map[string]any rather than []byte so consumers don't
// each have to re-parse JSON. UpdatedAt is advisory: it reflects
// when the row was last written, not when its content was generated
// or validated.
type ProviderConfigEntry struct {
	Source    string
	Alias     string
	Config    map[string]any
	UpdatedAt time.Time
}

// GetProviderConfig returns the persisted config for the given
// (source, alias) pair. Returns ErrConfigNotFound if no row exists.
//
// source must be the canonical registry address (e.g.
// "registry.terraform.io/hashicorp/aws"); alias is the empty string
// for unaliased default configurations. The store does no
// canonicalization — callers own that, typically by routing through
// provider.ParseSourceAddress.
func (s *Store) GetProviderConfig(ctx context.Context, source, alias string) (*ProviderConfigEntry, error) {
	const q = `
		SELECT config_jsonb, updated_at
		FROM   provider_configs
		WHERE  provider_source = $1
		  AND  alias           = $2
	`
	var (
		raw       []byte
		updatedAt time.Time
	)
	err := s.pool.QueryRow(ctx, q, source, alias).Scan(&raw, &updatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrConfigNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("query provider_configs: %w", err)
	}

	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("decode config for %s[%q]: %w", source, alias, err)
	}
	if cfg == nil {
		// JSONB stored a literal `null`. The CHECK constraint should
		// prevent this, but be defensive — Postgres CHECKs do not
		// run against pre-existing rows after a constraint is added.
		cfg = map[string]any{}
	}

	return &ProviderConfigEntry{
		Source:    source,
		Alias:     alias,
		Config:    cfg,
		UpdatedAt: updatedAt,
	}, nil
}

// PutProviderConfig upserts a config row for the given (source,
// alias) pair. ON CONFLICT replaces config_jsonb and bumps
// updated_at — same semantics as PutProviderSchema.
//
// source must be non-empty; an empty alias means "default
// (unaliased) configuration", which is the common case. config
// must be non-nil. An empty map is fine and represents a provider
// like null that takes no configuration; callers wanting to
// distinguish "no config recorded" from "explicitly empty config"
// should use Delete instead.
func (s *Store) PutProviderConfig(ctx context.Context, source, alias string, config map[string]any) error {
	if source == "" {
		return errors.New("PutProviderConfig: source must not be empty")
	}
	if config == nil {
		return errors.New("PutProviderConfig: config must not be nil (use empty map for no-attributes)")
	}

	raw, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("encode config for %s[%q]: %w", source, alias, err)
	}

	const upsert = `
		INSERT INTO provider_configs
			(provider_source, alias, config_jsonb, updated_at)
		VALUES
			($1, $2, $3, now())
		ON CONFLICT (provider_source, alias) DO UPDATE
			SET config_jsonb = EXCLUDED.config_jsonb,
			    updated_at   = EXCLUDED.updated_at
	`
	if _, err := s.pool.Exec(ctx, upsert, source, alias, raw); err != nil {
		return fmt.Errorf("upsert provider_configs: %w", err)
	}
	return nil
}

// DeleteProviderConfig removes the row for (source, alias). Returns
// the number of rows deleted (0 = already absent, 1 = deleted).
// Idempotent: deleting a missing row is not an error.
func (s *Store) DeleteProviderConfig(ctx context.Context, source, alias string) (int64, error) {
	const del = `DELETE FROM provider_configs WHERE provider_source = $1 AND alias = $2`
	tag, err := s.pool.Exec(ctx, del, source, alias)
	if err != nil {
		return 0, fmt.Errorf("delete provider_configs: %w", err)
	}
	return tag.RowsAffected(), nil
}

// ListProviderConfigs enumerates every stored config in stable
// order (source, then alias). Suitable for `kl provider
// list` UX. The full attribute map is decoded for every row; this
// is fine in v1.5b because v1 deployments typically have on the
// order of 10s of provider configs, not 10s of thousands. If that
// ever changes, switch to a paged interface.
func (s *Store) ListProviderConfigs(ctx context.Context) ([]ProviderConfigEntry, error) {
	const q = `
		SELECT provider_source, alias, config_jsonb, updated_at
		FROM   provider_configs
		ORDER BY provider_source, alias
	`
	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("query provider_configs: %w", err)
	}
	defer rows.Close()

	var out []ProviderConfigEntry
	for rows.Next() {
		var (
			source, alias string
			raw           []byte
			updatedAt     time.Time
		)
		if err := rows.Scan(&source, &alias, &raw, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan provider_configs: %w", err)
		}
		var cfg map[string]any
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, fmt.Errorf("decode config for %s[%q]: %w", source, alias, err)
		}
		if cfg == nil {
			cfg = map[string]any{}
		}
		out = append(out, ProviderConfigEntry{
			Source:    source,
			Alias:     alias,
			Config:    cfg,
			UpdatedAt: updatedAt,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate provider_configs: %w", err)
	}
	return out, nil
}
