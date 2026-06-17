-- 0014_environment_resource_quota.sql

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
