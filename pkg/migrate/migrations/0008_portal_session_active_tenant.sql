-- 0008_portal_session_active_tenant.sql

BEGIN;

ALTER TABLE portal_sessions
    ADD COLUMN IF NOT EXISTS active_tenant_slug text REFERENCES tenants(slug) ON DELETE SET NULL;

COMMIT;
