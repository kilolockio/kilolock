-- 0009_ownership_transfer_proposals.sql

BEGIN;

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
