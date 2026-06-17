-- 0013_environments.sql
-- Environment-scoped isolation (ADR 0013). Each environment is a deployable
-- unit; API tokens bind to one environment. database_dsn NULL means "use the
-- server's default connection" (unified / self-hosted mode).

BEGIN;

CREATE TABLE environments (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     uuid        NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    slug          text        NOT NULL,
    tier          text        NOT NULL DEFAULT 'shared_host'
        CHECK (tier IN ('shared_host', 'dedicated_host')),
    status        text        NOT NULL DEFAULT 'ready'
        CHECK (status IN ('provisioning', 'ready', 'failed')),
    database_name text,
    database_dsn  text,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT environments_tenant_slug_key UNIQUE (tenant_id, slug)
);

CREATE INDEX environments_tenant_idx ON environments (tenant_id);
CREATE INDEX environments_status_idx ON environments (status) WHERE status != 'ready';

-- One default environment per existing tenant.
INSERT INTO environments (tenant_id, slug, tier, status, database_name)
SELECT id, 'default', 'shared_host', 'ready', NULL
FROM   tenants;

ALTER TABLE api_tokens
    ADD COLUMN environment_id uuid REFERENCES environments(id);

UPDATE api_tokens tok
SET    environment_id = e.id
FROM   environments e
WHERE  e.tenant_id = tok.tenant_id
  AND  e.slug = 'default';

ALTER TABLE api_tokens
    ALTER COLUMN environment_id SET NOT NULL;

ALTER TABLE api_tokens DROP CONSTRAINT api_tokens_tenant_name_key;
ALTER TABLE api_tokens
    ADD CONSTRAINT api_tokens_environment_name_key UNIQUE (environment_id, name);

CREATE INDEX api_tokens_environment_idx ON api_tokens (environment_id);

INSERT INTO schema_migrations (version) VALUES (13)
    ON CONFLICT (version) DO NOTHING;

COMMIT;
