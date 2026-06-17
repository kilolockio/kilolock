package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kilolockio/kilolock/internal/provider"
)

// ErrSchemaNotCached is returned by GetProviderSchema when no entry
// exists for the requested (source, version). Callers should treat
// this as the cue to perform a live GetSchema RPC and then
// PutProviderSchema the result.
var ErrSchemaNotCached = errors.New("provider schema not cached")

// ProviderSchemaEntry is a single cached schema row, joined with its
// decoded Go representation. FetchedAt is the wall-clock time at
// which the schema was originally pulled from the provider; staleness
// policy is the caller's concern.
type ProviderSchemaEntry struct {
	Source          string
	Version         string
	ProtocolVersion int
	Schema          *provider.Schema
	FetchedAt       time.Time
}

// GetProviderSchema returns the cached schema entry for the given
// (source, version) pair. Returns ErrSchemaNotCached if no row
// exists. Callers can errors.Is to detect this case cleanly.
//
// source is the canonical registry address (e.g.
// "registry.terraform.io/hashicorp/null"); version is the resolved
// semver (e.g. "3.3.0"). Both are case-sensitive; the store does no
// normalization. Provider discovery code (added in a later commit)
// owns canonicalization.
func (s *Store) GetProviderSchema(ctx context.Context, source, version string) (*ProviderSchemaEntry, error) {
	const q = `
		SELECT protocol_version, schema_jsonb, fetched_at
		FROM   provider_schemas
		WHERE  provider_source  = $1
		  AND  provider_version = $2
	`
	var (
		protocolVersion int16
		raw             []byte
		fetchedAt       time.Time
	)
	err := s.pool.QueryRow(ctx, q, source, version).Scan(&protocolVersion, &raw, &fetchedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrSchemaNotCached
	}
	if err != nil {
		return nil, fmt.Errorf("query provider_schemas: %w", err)
	}

	out := &provider.Schema{}
	if err := json.Unmarshal(raw, out); err != nil {
		return nil, fmt.Errorf("decode cached schema for %s@%s: %w", source, version, err)
	}

	return &ProviderSchemaEntry{
		Source:          source,
		Version:         version,
		ProtocolVersion: int(protocolVersion),
		Schema:          out,
		FetchedAt:       fetchedAt,
	}, nil
}

// PutProviderSchema inserts or updates the cached schema for the
// given (source, version) pair. ON CONFLICT updates protocol_version,
// schema_jsonb, and fetched_at — equivalent to "evict and re-cache"
// without a separate Invalidate call. Safe to call concurrently from
// multiple goroutines or processes; Postgres serializes the
// per-row upsert.
//
// schema must be non-nil; an empty Schema struct is fine.
// protocolVersion must be 5 or 6 (the DB constraint will reject other
// values, but this returns a cleaner error early).
func (s *Store) PutProviderSchema(ctx context.Context, source, version string, protocolVersion int, schema *provider.Schema) error {
	if schema == nil {
		return errors.New("PutProviderSchema: schema must not be nil")
	}
	if protocolVersion != 5 && protocolVersion != 6 {
		return fmt.Errorf("PutProviderSchema: protocol version must be 5 or 6, got %d", protocolVersion)
	}

	raw, err := json.Marshal(schema)
	if err != nil {
		return fmt.Errorf("encode schema for %s@%s: %w", source, version, err)
	}

	const upsert = `
		INSERT INTO provider_schemas
			(provider_source, provider_version, protocol_version, schema_jsonb, fetched_at)
		VALUES
			($1, $2, $3, $4, now())
		ON CONFLICT (provider_source, provider_version) DO UPDATE
			SET protocol_version = EXCLUDED.protocol_version,
			    schema_jsonb     = EXCLUDED.schema_jsonb,
			    fetched_at       = EXCLUDED.fetched_at
	`
	if _, err := s.pool.Exec(ctx, upsert, source, version, int16(protocolVersion), raw); err != nil {
		return fmt.Errorf("upsert provider_schemas: %w", err)
	}
	return nil
}

// InvalidateProviderSchema deletes the cached row for (source, version).
// Returns the number of rows deleted (0 if the entry was already
// missing, 1 on success). No error is returned for the "already
// missing" case — invalidation is idempotent.
func (s *Store) InvalidateProviderSchema(ctx context.Context, source, version string) (int64, error) {
	const del = `DELETE FROM provider_schemas WHERE provider_source = $1 AND provider_version = $2`
	tag, err := s.pool.Exec(ctx, del, source, version)
	if err != nil {
		return 0, fmt.Errorf("delete provider_schemas: %w", err)
	}
	return tag.RowsAffected(), nil
}
