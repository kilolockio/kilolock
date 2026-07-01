-- Speed up state-engine scope expansion on large states.
--
-- Current hot path:
--   SELECT ... FROM resources
--   WHERE state_id = $1 AND delete_serial IS NULL
--   ORDER BY address
--
-- The existing partial index on (state_id) WHERE delete_serial IS NULL helps
-- prune to live rows, but it does not help the ORDER BY address shape. This
-- composite partial index lets Postgres walk the current/live resources of one
-- state in address order directly, which is especially helpful once states
-- grow into 100k+ resource territory.
--
-- We intentionally avoid plain `CREATE INDEX IF NOT EXISTS` here. Some restored
-- or partially-upgraded databases can reach a catalog state where the relname
-- already exists, yet Postgres still raises a duplicate-key error during
-- CREATE INDEX. The explicit catalog check plus exception guard makes the
-- migration safe to retry on those databases.

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_class c
        JOIN pg_namespace n ON n.oid = c.relnamespace
        WHERE c.relkind = 'i'
          AND c.relname = 'resources_state_open_address_idx'
          AND n.nspname = current_schema()
    ) THEN
        BEGIN
            EXECUTE $sql$
                CREATE INDEX resources_state_open_address_idx
                    ON resources (state_id, address)
                    WHERE delete_serial IS NULL
            $sql$;
        EXCEPTION
            WHEN duplicate_table OR duplicate_object OR unique_violation THEN
                NULL;
        END;
    END IF;
END
$$;
