-- 0021_workspace_env_public_ids.sql

ALTER TABLE tenants
    ADD COLUMN IF NOT EXISTS workspace_id text;

UPDATE tenants
SET workspace_id = 'ws_' || substr(replace(id::text, '-', ''), 1, 12)
WHERE COALESCE(workspace_id, '') = '';

ALTER TABLE tenants
    ALTER COLUMN workspace_id SET DEFAULT ('ws_' || substr(replace(gen_random_uuid()::text, '-', ''), 1, 12));

ALTER TABLE tenants
    ALTER COLUMN workspace_id SET NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS tenants_workspace_id_key
    ON tenants (workspace_id);

ALTER TABLE environments
    ADD COLUMN IF NOT EXISTS env_public_id text;

UPDATE environments
SET env_public_id = 'env_' || substr(replace(id::text, '-', ''), 1, 12)
WHERE COALESCE(env_public_id, '') = '';

ALTER TABLE environments
    ALTER COLUMN env_public_id SET DEFAULT ('env_' || substr(replace(gen_random_uuid()::text, '-', ''), 1, 12));

ALTER TABLE environments
    ALTER COLUMN env_public_id SET NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS environments_env_public_id_key
    ON environments (env_public_id);

