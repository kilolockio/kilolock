-- 0014_dedicated_host.sql
-- Metadata for dedicated Cloud SQL instances (ADR 0013 E6).

BEGIN;

ALTER TABLE environments
    ADD COLUMN host_connection_name text,
    ADD COLUMN source_database_dsn  text,
    ADD COLUMN provision_error      text,
    ADD COLUMN provision_started_at timestamptz,
    ADD COLUMN provision_finished_at timestamptz;

CREATE INDEX environments_provisioning_idx
    ON environments (status, tier)
    WHERE status = 'provisioning';

INSERT INTO schema_migrations (version) VALUES (14)
    ON CONFLICT (version) DO NOTHING;

COMMIT;
