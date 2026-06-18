-- 0030_environment_state_lock_defaults.sql
-- Environment-level defaults for new state lock behavior.

BEGIN;

ALTER TABLE environments
    ADD COLUMN IF NOT EXISTS state_lock_default_mode text NOT NULL DEFAULT 'auto';

ALTER TABLE environments
    DROP CONSTRAINT IF EXISTS environments_state_lock_default_mode_check;

ALTER TABLE environments
    ADD CONSTRAINT environments_state_lock_default_mode_check
    CHECK (state_lock_default_mode IN ('auto', 'vanilla', 'kilolock'));

COMMIT;
