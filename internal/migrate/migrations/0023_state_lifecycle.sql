ALTER TABLE states
    ADD COLUMN IF NOT EXISTS lifecycle_status text NOT NULL DEFAULT 'active';

ALTER TABLE states
    ADD COLUMN IF NOT EXISTS lifecycle_changed_at timestamptz;

ALTER TABLE states
    ADD COLUMN IF NOT EXISTS lifecycle_changed_by text NOT NULL DEFAULT '';

ALTER TABLE states
    ADD COLUMN IF NOT EXISTS lifecycle_reason text NOT NULL DEFAULT '';

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conname = 'states_lifecycle_status_check'
    ) THEN
        ALTER TABLE states
            ADD CONSTRAINT states_lifecycle_status_check
            CHECK (lifecycle_status IN ('active', 'suspended', 'archived'));
    END IF;
END $$;
