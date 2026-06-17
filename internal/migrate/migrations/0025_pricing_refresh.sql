-- 0025_pricing_refresh.sql

ALTER TABLE tenants
    ALTER COLUMN billing_plan SET DEFAULT 'starter',
    ALTER COLUMN max_environments SET DEFAULT 0,
    ALTER COLUMN max_state_resources SET DEFAULT 0,
    ALTER COLUMN max_environment_resources SET DEFAULT 0;

UPDATE tenants
SET billing_plan = 'starter',
    max_environments = 0,
    max_state_resources = 0,
    max_environment_resources = 0
WHERE billing_plan IN ('starter_5', 'starter');
