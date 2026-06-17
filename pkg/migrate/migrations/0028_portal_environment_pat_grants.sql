-- 0028_portal_environment_pat_grants.sql

BEGIN;

CREATE TABLE IF NOT EXISTS portal_environment_pat_grants (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id   uuid NOT NULL REFERENCES portal_accounts(id) ON DELETE CASCADE,
    environment_id uuid NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
    granted_by   text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now(),
    revoked_at   timestamptz,
    revoked_by   text NOT NULL DEFAULT ''
);

CREATE UNIQUE INDEX IF NOT EXISTS portal_environment_pat_grants_active_uq
    ON portal_environment_pat_grants (account_id, environment_id)
    WHERE revoked_at IS NULL;

CREATE INDEX IF NOT EXISTS portal_environment_pat_grants_environment_idx
    ON portal_environment_pat_grants (environment_id, created_at DESC);

CREATE INDEX IF NOT EXISTS portal_environment_pat_grants_account_idx
    ON portal_environment_pat_grants (account_id, created_at DESC);

COMMIT;
