-- Tickets made by another tool remember where they came from (Bugfixes: its project and report),
-- so Tracklines can show the source's live details. Org integrations hold the credentials for
-- those calls, sealed with the server's key (secrets package); the raw secret is never returned.
ALTER TABLE tickets
    ADD COLUMN external_source  TEXT, -- e.g. bugfixes
    ADD COLUMN external_project TEXT, -- the source's project id
    ADD COLUMN external_id      TEXT, -- e.g. the Bugfixes report id
    ADD COLUMN external_url     TEXT;
CREATE INDEX tickets_external_idx ON tickets (external_source, external_project, external_id) WHERE external_source IS NOT NULL;

CREATE TABLE org_integrations (
    org_id     TEXT NOT NULL,
    provider   TEXT NOT NULL,        -- bugfixes (github, bitbucket later)
    base_url   TEXT NOT NULL DEFAULT '',
    secret     BYTEA NOT NULL,       -- sealed (AES-GCM), see internal/secrets
    updated_by TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, provider)
);
