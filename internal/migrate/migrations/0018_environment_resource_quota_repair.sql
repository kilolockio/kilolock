-- 0018_environment_resource_quota_repair.sql
--
-- Repair POC/local databases created from an older squashed baseline where
-- schema_migrations already recorded versions 0001..0017 as applied, but the
-- tenants table still predates max_environment_resources. Because
-- 0001_baseline.sql is only executed once, later edits to the baseline do not
-- repair existing databases; this forward-only migration does.

BEGIN;

ALTER TABLE tenants
    ADD COLUMN IF NOT EXISTS max_environment_resources integer NOT NULL DEFAULT 15000;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conname = 'tenants_max_environment_resources_positive'
          AND conrelid = 'tenants'::regclass
    ) THEN
        EXECUTE 'ALTER TABLE tenants ADD CONSTRAINT tenants_max_environment_resources_positive CHECK (max_environment_resources >= 0)';
    END IF;
END
$$;

COMMIT;
