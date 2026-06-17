-- 0011_personal_workspaces.sql

BEGIN;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM information_schema.columns
        WHERE table_schema = 'public'
          AND table_name = 'tenants'
          AND column_name = 'kind'
    ) THEN
        EXECUTE 'ALTER TABLE tenants ADD COLUMN kind text';
    END IF;

    EXECUTE 'UPDATE tenants SET kind = ''organization'' WHERE kind IS NULL';
    EXECUTE 'ALTER TABLE tenants ALTER COLUMN kind SET DEFAULT ''organization''';
    EXECUTE 'ALTER TABLE tenants ALTER COLUMN kind SET NOT NULL';

    IF NOT EXISTS (
        SELECT 1
        FROM information_schema.columns
        WHERE table_schema = 'public'
          AND table_name = 'tenants'
          AND column_name = 'personal_owner_account_id'
    ) THEN
        EXECUTE 'ALTER TABLE tenants ADD COLUMN personal_owner_account_id uuid';
    END IF;

    IF to_regclass('public.portal_accounts') IS NOT NULL
       AND NOT EXISTS (
           SELECT 1
           FROM pg_constraint
           WHERE conname = 'tenants_personal_owner_account_id_fkey'
             AND conrelid = 'tenants'::regclass
       ) THEN
        EXECUTE 'ALTER TABLE tenants ADD CONSTRAINT tenants_personal_owner_account_id_fkey FOREIGN KEY (personal_owner_account_id) REFERENCES portal_accounts(id) ON DELETE SET NULL';
    END IF;
END
$$;

DO $$
BEGIN
    IF to_regclass('public.tenants_personal_owner_account_uq') IS NULL THEN
        EXECUTE 'CREATE UNIQUE INDEX tenants_personal_owner_account_uq ON tenants (personal_owner_account_id) WHERE personal_owner_account_id IS NOT NULL';
    END IF;
END
$$;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conname = 'tenants_kind_valid'
          AND conrelid = 'tenants'::regclass
    ) THEN
        EXECUTE 'ALTER TABLE tenants ADD CONSTRAINT tenants_kind_valid CHECK (kind IN (''organization'',''personal''))';
    END IF;
END
$$;

COMMIT;
