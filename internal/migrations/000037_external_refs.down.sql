DROP TABLE org_integrations;
ALTER TABLE tickets DROP COLUMN external_source, DROP COLUMN external_project, DROP COLUMN external_id, DROP COLUMN external_url;
