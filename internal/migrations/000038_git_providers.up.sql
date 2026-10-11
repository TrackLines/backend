-- GitHub (App installation) and Bitbucket (OAuth) connections live in org_integrations like
-- Bugfixes; account is who/where they're connected as (GitHub org or user, Bitbucket user).
-- A project links the repos its tickets' PRs come from; a repo belongs to one project per org.
-- external_id is the provider's id for the connection (GitHub installation id), so webhooks can find it.
ALTER TABLE org_integrations ADD COLUMN account TEXT NOT NULL DEFAULT '', ADD COLUMN external_id TEXT NOT NULL DEFAULT '';
CREATE INDEX org_integrations_external_idx ON org_integrations (provider, external_id) WHERE external_id <> '';

CREATE TABLE project_repos (
    project_id     UUID NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    org_id         TEXT NOT NULL,
    provider       TEXT NOT NULL CHECK (provider IN ('github', 'bitbucket')),
    full_name      TEXT NOT NULL, -- owner/repo (GitHub), workspace/repo (Bitbucket)
    default_branch TEXT NOT NULL DEFAULT 'main',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, provider, full_name),
    UNIQUE (org_id, provider, full_name)
);
