-- 0012_portal_accounts_repair.sql
-- Repair drifted local/POC databases where schema_migrations may claim the
-- portal account migration is applied but the replacement tables are absent.

BEGIN;

CREATE TABLE IF NOT EXISTS portal_accounts (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email          text NOT NULL UNIQUE,
    company        text NOT NULL DEFAULT '',
    plan           text NOT NULL DEFAULT 'starter',
    password_salt  text NOT NULL,
    password_hash  text NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS tenant_memberships (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_slug    text NOT NULL REFERENCES tenants(slug) ON DELETE CASCADE,
    account_id     uuid NOT NULL REFERENCES portal_accounts(id) ON DELETE CASCADE,
    role           text NOT NULL DEFAULT 'member' CHECK (role IN ('owner', 'tenant_admin', 'billing_admin', 'member')),
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    revoked_at     timestamptz,
    revoked_by     text NOT NULL DEFAULT ''
);

CREATE UNIQUE INDEX IF NOT EXISTS tenant_memberships_active_uq
    ON tenant_memberships (tenant_slug, account_id)
    WHERE revoked_at IS NULL;

CREATE INDEX IF NOT EXISTS tenant_memberships_tenant_idx
    ON tenant_memberships (tenant_slug, created_at);

CREATE INDEX IF NOT EXISTS tenant_memberships_account_idx
    ON tenant_memberships (account_id, created_at);

CREATE TABLE IF NOT EXISTS portal_sessions (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id         uuid NOT NULL REFERENCES portal_accounts(id) ON DELETE CASCADE,
    active_tenant_slug text REFERENCES tenants(slug) ON DELETE SET NULL,
    token_hash         text NOT NULL UNIQUE,
    expires_at         timestamptz NOT NULL,
    created_at         timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS portal_sessions_account_idx
    ON portal_sessions (account_id);

CREATE INDEX IF NOT EXISTS portal_sessions_expires_idx
    ON portal_sessions (expires_at);

COMMIT;
