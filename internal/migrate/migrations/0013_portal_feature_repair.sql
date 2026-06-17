-- 0013_portal_feature_repair.sql
-- Repair drifted local/POC databases where newer portal feature migrations
-- were marked applied historically but their tables/columns are absent.

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

CREATE INDEX IF NOT EXISTS tenant_invitations_tenant_idx
    ON tenant_invitations (tenant_slug, created_at DESC);

CREATE INDEX IF NOT EXISTS tenant_invitations_email_idx
    ON tenant_invitations (email, created_at DESC);

ALTER TABLE portal_sessions
    ADD COLUMN IF NOT EXISTS active_tenant_slug text REFERENCES tenants(slug) ON DELETE SET NULL;

CREATE TABLE IF NOT EXISTS ownership_transfer_proposals (
    id                      uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    resource_type           text NOT NULL CHECK (resource_type IN ('environment')),
    resource_id             uuid NOT NULL,
    resource_name           text NOT NULL DEFAULT '',
    current_owner_kind      text NOT NULL CHECK (current_owner_kind IN ('tenant')),
    current_owner_ref       text NOT NULL,
    target_owner_kind       text NOT NULL CHECK (target_owner_kind IN ('tenant')),
    target_owner_ref        text NOT NULL,
    billing_impact          boolean NOT NULL DEFAULT true,
    status                  text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'accepted', 'rejected', 'cancelled', 'expired')),
    initiated_by_account_id uuid REFERENCES portal_accounts(id) ON DELETE SET NULL,
    initiated_by            text NOT NULL DEFAULT '',
    initiated_reason        text NOT NULL DEFAULT '',
    accepted_by_account_id  uuid REFERENCES portal_accounts(id) ON DELETE SET NULL,
    accepted_by             text NOT NULL DEFAULT '',
    accepted_at             timestamptz,
    rejected_by_account_id  uuid REFERENCES portal_accounts(id) ON DELETE SET NULL,
    rejected_by             text NOT NULL DEFAULT '',
    rejected_at             timestamptz,
    cancelled_by_account_id uuid REFERENCES portal_accounts(id) ON DELETE SET NULL,
    cancelled_by            text NOT NULL DEFAULT '',
    cancelled_at            timestamptz,
    expires_at              timestamptz,
    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS ownership_transfer_proposals_resource_idx
    ON ownership_transfer_proposals (resource_type, resource_id, created_at DESC);

CREATE INDEX IF NOT EXISTS ownership_transfer_proposals_current_owner_idx
    ON ownership_transfer_proposals (current_owner_ref, created_at DESC);

CREATE INDEX IF NOT EXISTS ownership_transfer_proposals_target_owner_idx
    ON ownership_transfer_proposals (target_owner_ref, created_at DESC);

COMMIT;
