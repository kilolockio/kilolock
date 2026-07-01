-- Preserve Terraform/provider per-instance schema_version in normalized
-- resource rows. Without this, canonical state rebuilds can rewrite a resource
-- instance as schema_version=0, which breaks providers that expect upgrade
-- ladders to start from a later stored version.

ALTER TABLE resources
    ADD COLUMN IF NOT EXISTS schema_version integer NOT NULL DEFAULT 0;

INSERT INTO schema_migrations (version) VALUES (33)
    ON CONFLICT (version) DO NOTHING;
