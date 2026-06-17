-- 0004_provider_configs.sql
-- v1 step 2 (5b): persisted provider configuration blocks. Companion
-- to the wire-level Configure RPC added in v1.5a — that commit shipped
-- the ability to call ConfigureProvider but left "where does the
-- config come from" unanswered. This commit answers it.
--
-- Each row is one (provider_source, alias) pair with its attribute
-- map stored as JSONB. The schema deliberately mirrors the storage
-- model used for provider_schemas (0003): JSONB column owned by the
-- Go layer, no SQL-side validation of structure beyond presence and
-- shape (object, non-null). The Go layer marshals/unmarshals via
-- encoding/json; consumers should not query into config_jsonb from
-- SQL.
--
-- Why support an alias column at all in v1?
--
-- Terraform's HCL allows the same provider to be declared multiple
-- times with different configurations:
--
--   provider "aws" { region = "us-east-1" }
--   provider "aws" { alias = "west"; region = "us-west-2" }
--
-- Resources reference these via `provider = aws.west`. State files
-- record the alias as part of the provider address. v1 refresh has
-- to look up the right config for each resource, so the table is
-- keyed on (source, alias) from day one. Most rows will have an
-- empty alias.

BEGIN;

CREATE TABLE provider_configs (
    provider_source TEXT        NOT NULL,
    alias           TEXT        NOT NULL DEFAULT '',
    config_jsonb    JSONB       NOT NULL,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (provider_source, alias),

    -- Defensive: a config block is structurally a (possibly empty)
    -- object. Storing arrays, scalars, or null at the top level
    -- would silently break encoding at refresh time; reject early.
    CONSTRAINT provider_configs_is_object
        CHECK (jsonb_typeof(config_jsonb) = 'object')
);

-- Listing all aliases for a given source is the common UX (e.g.
-- `kl provider list hashicorp/aws`); the primary key
-- already supports this via prefix scan. No extra indexes in v1.5b.

COMMENT ON TABLE  provider_configs IS 'Persisted provider configuration blocks keyed by (source, alias); see v1.5 commits.';
COMMENT ON COLUMN provider_configs.provider_source IS 'Canonical registry address, e.g. registry.terraform.io/hashicorp/aws.';
COMMENT ON COLUMN provider_configs.alias IS 'HCL alias (empty string = default/unaliased config).';
COMMENT ON COLUMN provider_configs.config_jsonb IS 'Attribute map for the provider config block. Opaque to SQL; owned by the Go layer.';
COMMENT ON COLUMN provider_configs.updated_at IS 'Last write time; advisory, not used for cache eviction.';

INSERT INTO schema_migrations (version) VALUES (4)
    ON CONFLICT (version) DO NOTHING;

COMMIT;
