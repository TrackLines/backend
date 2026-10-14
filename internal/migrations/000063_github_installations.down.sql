-- back to one connection per org and provider: keep each org's most recent GitHub account
DELETE FROM org_integrations o USING org_integrations newer
WHERE o.provider = 'github' AND newer.provider = 'github' AND o.org_id = newer.org_id
  AND (newer.updated_at, newer.external_id) > (o.updated_at, o.external_id);
ALTER TABLE org_integrations DROP CONSTRAINT org_integrations_pkey;
ALTER TABLE org_integrations ADD PRIMARY KEY (org_id, provider);
