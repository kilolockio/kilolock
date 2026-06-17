-- 0003_provider_schemas.sql
-- v1 step 1: cache provider schemas in Postgres so the refresh path
-- does not pay GetSchema RPC cost on every invocation. See ADR 0005.
--
-- Each row is the full schema as returned by a provider at a specific
-- version, keyed by (provider_source, provider_version). The source
-- is the canonical registry address (e.g. "registry.terraform.io/
-- hashicorp/null"); the version is the resolved semver Terraform's
-- lock file would pin (e.g. "3.3.0"). Together they uniquely identify
-- a provider binary's schema.
--
-- The schema itself is stored as JSONB. The shape mirrors the
-- internal/provider.Schema Go struct (encoding/json round-trip);
-- consumers are expected to decode it via the provider package's
-- helpers, not query the JSONB structure from SQL. That keeps us
-- free to evolve the in-memory schema model without coordinating
-- SQL-level changes.

BEGIN;

CREATE TABLE provider_schemas (
    provider_source   TEXT        NOT NULL,
    provider_version  TEXT        NOT NULL,
    protocol_version  SMALLINT    NOT NULL,
    schema_jsonb      JSONB       NOT NULL,
    fetched_at        TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (provider_source, provider_version),

    -- Defensive: this is the wire-level protocol version negotiated
    -- when the schema was fetched. Anything other than 5 or 6 is a
    -- programming error; the check ensures bad data never lands
    -- silently.
    CONSTRAINT provider_schemas_protocol_version_valid
        CHECK (protocol_version IN (5, 6))
);

-- Provider source listing across pinned versions is rare; the
-- primary key already covers exact lookups. No extra indexes needed
-- in v1.1. If `kl providers ls` ever needs to enumerate by
-- source alone, a future commit can add an index on provider_source.

COMMENT ON TABLE  provider_schemas IS 'Cached provider GetSchema responses keyed by (source, version); see ADR 0005.';
COMMENT ON COLUMN provider_schemas.provider_source IS 'Canonical registry address, e.g. registry.terraform.io/hashicorp/null.';
COMMENT ON COLUMN provider_schemas.provider_version IS 'Resolved semver of the provider binary, e.g. 3.3.0.';
COMMENT ON COLUMN provider_schemas.protocol_version IS 'Wire-level plugin protocol negotiated when this schema was fetched (5 or 6).';
COMMENT ON COLUMN provider_schemas.schema_jsonb IS 'JSON-encoded provider.Schema as defined in internal/provider/client.go. Opaque to SQL.';

INSERT INTO schema_migrations (version) VALUES (3)
    ON CONFLICT (version) DO NOTHING;

COMMIT;
