-- 0003_billing_admin_role_and_portal_user_roles.sql
-- Introduce billing_admin RBAC permission/role and portal user roles.

BEGIN;

DO $$
BEGIN
    IF to_regclass('portal_users') IS NOT NULL THEN
        EXECUTE $sql$
            ALTER TABLE portal_users
                ADD COLUMN IF NOT EXISTS role text NOT NULL DEFAULT 'member'
                CHECK (role IN ('owner', 'billing_admin', 'member'))
        $sql$;
    END IF;
END
$$;

-- RBAC: billing checkout permission + role.
INSERT INTO rbac_permissions (key, description) VALUES
    ('tenant.billing.checkout', 'Create Stripe checkout sessions / manage tenant billing.')
ON CONFLICT (key) DO NOTHING;

INSERT INTO rbac_roles (key, name, description) VALUES
    ('billing_admin', 'Billing admin', 'Can manage billing for a tenant (checkout, payment methods).')
ON CONFLICT (key) DO NOTHING;

WITH pairs(role_key, perm_key) AS (
    VALUES
      ('billing_admin', 'tenant.billing.checkout'),
      ('platform_admin', 'tenant.billing.checkout')
)
INSERT INTO rbac_role_permissions (role_id, permission_id)
SELECT r.id, p.id
FROM pairs
JOIN rbac_roles r ON r.key = pairs.role_key
JOIN rbac_permissions p ON p.key = pairs.perm_key
ON CONFLICT (role_id, permission_id) DO NOTHING;

COMMIT;
