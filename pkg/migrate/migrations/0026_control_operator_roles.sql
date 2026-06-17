-- Add richer control-operator roles for support and security workflows.

INSERT INTO rbac_roles (key, name, description) VALUES
    ('support_admin', 'Support admin', 'Support operator with recovery/lifecycle powers but no billing or RBAC management.'),
    ('security_admin', 'Security admin', 'Security operator who can manage control tokens and RBAC assignments.')
ON CONFLICT (key) DO NOTHING;

WITH pairs(role_key, perm_key) AS (
    VALUES
      ('support_admin', 'tenant.read'),
      ('support_admin', 'state.config.update'),
      ('support_admin', 'state.delete'),
      ('support_admin', 'environment.read'),
      ('support_admin', 'environment.lifecycle.update'),
      ('support_admin', 'token.read'),
      ('security_admin', 'tenant.read'),
      ('security_admin', 'environment.read'),
      ('security_admin', 'token.read'),
      ('security_admin', 'token.create'),
      ('security_admin', 'token.lifecycle.update'),
      ('security_admin', 'rbac.manage')
)
INSERT INTO rbac_role_permissions (role_id, permission_id)
SELECT r.id, p.id
FROM pairs
JOIN rbac_roles r ON r.key = pairs.role_key
JOIN rbac_permissions p ON p.key = pairs.perm_key
ON CONFLICT (role_id, permission_id) DO NOTHING;
