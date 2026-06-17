INSERT INTO rbac_permissions (key, description) VALUES
    ('state.delete', 'Delete a Terraform state snapshot and its history.')
ON CONFLICT (key) DO NOTHING;

WITH pairs(role_key, perm_key) AS (
    VALUES
      ('platform_admin', 'state.delete'),
      ('tenant_admin', 'state.delete')
)
INSERT INTO rbac_role_permissions (role_id, permission_id)
SELECT r.id, p.id
FROM pairs
JOIN rbac_roles r ON r.key = pairs.role_key
JOIN rbac_permissions p ON p.key = pairs.perm_key
ON CONFLICT (role_id, permission_id) DO NOTHING;
