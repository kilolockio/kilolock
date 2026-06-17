-- 0010_environment_transfer_permission.sql

BEGIN;

INSERT INTO rbac_permissions (key, description) VALUES
    ('environment.transfer.update', 'Create and resolve environment ownership transfer proposals.')
ON CONFLICT (key) DO NOTHING;

WITH pair AS (
    SELECT r.id AS role_id, p.id AS permission_id
    FROM rbac_roles r
    JOIN rbac_permissions p ON p.key = 'environment.transfer.update'
    WHERE r.key = 'platform_admin'
)
INSERT INTO rbac_role_permissions (role_id, permission_id)
SELECT role_id, permission_id
FROM pair
ON CONFLICT (role_id, permission_id) DO NOTHING;

COMMIT;
