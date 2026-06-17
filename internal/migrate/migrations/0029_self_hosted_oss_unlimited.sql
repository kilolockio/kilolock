-- 0029_self_hosted_oss_unlimited.sql
--
-- In the OSS/self-hosted repo, starter-style hosted quotas are the wrong
-- default. The self-hosted experience should allow large local demos and
-- operator-managed tenants without silently tripping hosted plan caps.
--
-- Keep the billing_plan metadata, but default all numeric caps to 0
-- (interpreted by the write path as unlimited).

ALTER TABLE tenants
    ALTER COLUMN max_environments SET DEFAULT 0,
    ALTER COLUMN max_state_resources SET DEFAULT 0,
    ALTER COLUMN max_environment_resources SET DEFAULT 0;

UPDATE tenants
SET max_environments = 0,
    max_state_resources = 0,
    max_environment_resources = 0
WHERE slug = 'self-hosted'
   OR billing_plan IN ('starter_5', 'starter');
