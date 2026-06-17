-- 0027_portal_personal_access_tokens.sql

BEGIN;

CREATE TABLE IF NOT EXISTS portal_personal_access_tokens (
    id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id           uuid NOT NULL REFERENCES portal_accounts(id) ON DELETE CASCADE,
    token_hash           bytea NOT NULL UNIQUE,
    token_prefix         text NOT NULL,
    created_at           timestamptz NOT NULL DEFAULT now(),
    last_used_at         timestamptz,
    revoked_at           timestamptz,
    revoked_by           text NOT NULL DEFAULT '',
    replaced_by_token_id uuid REFERENCES portal_personal_access_tokens(id) ON DELETE SET NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS portal_personal_access_tokens_active_uq
    ON portal_personal_access_tokens (account_id)
    WHERE revoked_at IS NULL;

CREATE INDEX IF NOT EXISTS portal_personal_access_tokens_account_idx
    ON portal_personal_access_tokens (account_id, created_at DESC);

COMMIT;
