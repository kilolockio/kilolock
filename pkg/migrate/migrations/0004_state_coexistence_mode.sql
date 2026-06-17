-- 0004_state_coexistence_mode.sql
-- Per-state policy for vanilla-terraform/v2-apply coexistence.

BEGIN;

ALTER TABLE states
    ADD COLUMN IF NOT EXISTS coexistence_mode text NOT NULL DEFAULT 'warn';

ALTER TABLE states
    DROP CONSTRAINT IF EXISTS states_coexistence_mode_check;

ALTER TABLE states
    ADD CONSTRAINT states_coexistence_mode_check
    CHECK (coexistence_mode IN ('warn', 'strict'));

COMMIT;

