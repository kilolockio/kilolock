-- 0007_tenant_invitations.sql

BEGIN;

CREATE TABLE IF NOT EXISTS tenant_invitations (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_slug  text NOT NULL REFERENCES tenants(slug) ON DELETE CASCADE,
    email        text NOT NULL,
    role         text NOT NULL DEFAULT 'member' CHECK (role IN ('tenant_admin', 'billing_admin', 'member')),
    status       text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'accepted', 'rejected', 'cancelled', 'expired')),
    invited_by   text NOT NULL DEFAULT '',
    responded_at timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS tenant_invitations_tenant_idx ON tenant_invitations (tenant_slug, created_at DESC);
CREATE INDEX IF NOT EXISTS tenant_invitations_email_idx ON tenant_invitations (email, created_at DESC);

COMMIT;
