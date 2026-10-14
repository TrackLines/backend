-- An org can connect several GitHub accounts (TL-281): each project links repos from whichever
-- GitHub org owns them. A connection is now one row per installation; Bugfixes and Bitbucket have
-- no installation id (''), so they stay one per org. An installation still belongs to one org.
ALTER TABLE org_integrations DROP CONSTRAINT org_integrations_pkey;
ALTER TABLE org_integrations ADD PRIMARY KEY (org_id, provider, external_id);
