-- 0020_max_environments_default_2.sql

ALTER TABLE tenants
    ALTER COLUMN max_environments SET DEFAULT 2;

UPDATE tenants
SET max_environments = 2
WHERE max_environments = 1;

