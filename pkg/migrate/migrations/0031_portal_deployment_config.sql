-- 0031_portal_deployment_config.sql
-- Cloud portal deployment-wide configuration overrides.

BEGIN;

CREATE TABLE IF NOT EXISTS portal_deployment_config (
    id boolean PRIMARY KEY DEFAULT true CHECK (id),
    billing_enabled boolean NOT NULL DEFAULT true,
    backend_address_base_url text NOT NULL DEFAULT '',
    backend_lock_address_base_url text NOT NULL DEFAULT '',
    backend_unlock_address_base_url text NOT NULL DEFAULT '',
    backend_lock_method text NOT NULL DEFAULT 'LOCK',
    backend_unlock_method text NOT NULL DEFAULT 'POST',
    updated_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO portal_deployment_config (
    id,
    billing_enabled,
    backend_address_base_url,
    backend_lock_address_base_url,
    backend_unlock_address_base_url,
    backend_lock_method,
    backend_unlock_method
)
VALUES (
    true,
    true,
    '',
    '',
    '',
    'LOCK',
    'POST'
)
ON CONFLICT (id) DO NOTHING;

COMMIT;
