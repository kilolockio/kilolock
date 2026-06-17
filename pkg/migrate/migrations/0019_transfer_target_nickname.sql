-- 0019_transfer_target_nickname.sql

ALTER TABLE ownership_transfer_proposals
    ADD COLUMN IF NOT EXISTS target_resource_name text;

UPDATE ownership_transfer_proposals
SET target_resource_name = resource_name
WHERE COALESCE(target_resource_name, '') = '';

ALTER TABLE ownership_transfer_proposals
    ALTER COLUMN target_resource_name SET NOT NULL;

