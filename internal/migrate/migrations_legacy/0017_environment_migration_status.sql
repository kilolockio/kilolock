-- 0017_environment_migration_status.sql
-- Tracks per-environment migration outcomes for resilient startup.

BEGIN;

ALTER TABLE environments
    ADD COLUMN IF NOT EXISTS last_migration_version integer,
    ADD COLUMN IF NOT EXISTS last_migration_at timestamptz,
    ADD COLUMN IF NOT EXISTS last_migration_error text;

CREATE INDEX IF NOT EXISTS environments_last_migration_at_idx
    ON environments (last_migration_at);

INSERT INTO schema_migrations (version) VALUES (17)
    ON CONFLICT (version) DO NOTHING;

COMMIT;
