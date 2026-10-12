-- Existing encrypted secrets are re-sealed by the application on startup, where the key is available.
ALTER TABLE org_integrations ADD COLUMN secret_aad_version SMALLINT NOT NULL DEFAULT 0;
