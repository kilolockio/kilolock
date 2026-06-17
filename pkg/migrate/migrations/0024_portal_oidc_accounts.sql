ALTER TABLE portal_accounts
    ADD COLUMN IF NOT EXISTS password_login_enabled boolean NOT NULL DEFAULT true;

ALTER TABLE portal_accounts
    ADD COLUMN IF NOT EXISTS auth_source text NOT NULL DEFAULT 'password';

ALTER TABLE portal_accounts
    ADD COLUMN IF NOT EXISTS oidc_provider text NOT NULL DEFAULT '';

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conname = 'portal_accounts_auth_source_check'
    ) THEN
        ALTER TABLE portal_accounts
            ADD CONSTRAINT portal_accounts_auth_source_check
            CHECK (auth_source IN ('password', 'oidc'));
    END IF;
END $$;
