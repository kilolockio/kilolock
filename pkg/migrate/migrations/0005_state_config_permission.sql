-- Allow scoped control-plane updates of per-state concurrency/coexistence policy.

INSERT INTO rbac_permissions (key, description) VALUES
    ('state.config.update', 'Update per-state concurrency and coexistence policy.')
ON CONFLICT (key) DO NOTHING;

WITH pairs(role_key, perm_key) AS (
    VALUES
      ('platform_admin', 'state.config.update'),
      ('tenant_admin', 'state.config.update')
)
INSERT INTO rbac_role_permissions (role_id, permission_id)
SELECT r.id, p.id
FROM pairs
JOIN rbac_roles r ON r.key = pairs.role_key
JOIN rbac_permissions p ON p.key = pairs.perm_key
ON CONFLICT (role_id, permission_id) DO NOTHING;
