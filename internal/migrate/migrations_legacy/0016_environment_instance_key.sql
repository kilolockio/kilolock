-- 0016_environment_instance_key.sql
-- Adds data-plane instance routing key for environments.

BEGIN;

ALTER TABLE environments
    ADD COLUMN IF NOT EXISTS database_instance_key text;

UPDATE environments
SET    database_instance_key = 'shared'
WHERE  NULLIF(TRIM(COALESCE(database_instance_key, '')), '') IS NULL;

UPDATE environments
SET    database_instance_key = host_connection_name
WHERE  tier = 'dedicated_host'
  AND  NULLIF(TRIM(COALESCE(host_connection_name, '')), '') IS NOT NULL
  AND  database_instance_key = 'shared';

ALTER TABLE environments
    ALTER COLUMN database_instance_key SET DEFAULT 'shared';

ALTER TABLE environments
    ALTER COLUMN database_instance_key SET NOT NULL;

CREATE INDEX IF NOT EXISTS environments_instance_key_idx
    ON environments (database_instance_key);

INSERT INTO schema_migrations (version) VALUES (16)
    ON CONFLICT (version) DO NOTHING;

COMMIT;
