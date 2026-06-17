-- 0012_api_tokens.sql
-- Per-tenant API tokens for HTTP backend authentication.
--
-- Tokens are stored as SHA-256 hashes; the plaintext is shown once at
-- creation time. Each token belongs to exactly one tenant; the store
-- layer always scopes data by tenant_id from the resolved Principal.

BEGIN;

CREATE TABLE api_tokens (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    uuid        NOT NULL REFERENCES tenants(id),
    name         text        NOT NULL,
    token_hash   bytea       NOT NULL,
    token_prefix text        NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    revoked_at   timestamptz,
    last_used_at timestamptz,
    CONSTRAINT api_tokens_tenant_name_key UNIQUE (tenant_id, name)
);

-- Globally unique active token hashes (Bearer auth has no tenant hint).
CREATE UNIQUE INDEX api_tokens_active_hash_idx
    ON api_tokens (token_hash)
    WHERE revoked_at IS NULL;

CREATE INDEX api_tokens_tenant_idx ON api_tokens (tenant_id);

INSERT INTO schema_migrations (version) VALUES (12)
    ON CONFLICT (version) DO NOTHING;

COMMIT;
