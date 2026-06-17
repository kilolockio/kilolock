-- 0002_tenant_stripe_billing.sql
-- Stripe billing metadata + webhook idempotency.

BEGIN;

ALTER TABLE tenants
    ADD COLUMN IF NOT EXISTS stripe_customer_id text,
    ADD COLUMN IF NOT EXISTS stripe_subscription_id text,
    ADD COLUMN IF NOT EXISTS stripe_subscription_status text,
    ADD COLUMN IF NOT EXISTS stripe_price_id text,
    ADD COLUMN IF NOT EXISTS stripe_current_period_end timestamptz,
    ADD COLUMN IF NOT EXISTS stripe_updated_at timestamptz;

CREATE UNIQUE INDEX IF NOT EXISTS tenants_stripe_customer_id_uq
    ON tenants (stripe_customer_id)
    WHERE stripe_customer_id IS NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS tenants_stripe_subscription_id_uq
    ON tenants (stripe_subscription_id)
    WHERE stripe_subscription_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS stripe_webhook_events (
    id          text        PRIMARY KEY,
    type        text        NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now()
);

COMMIT;

