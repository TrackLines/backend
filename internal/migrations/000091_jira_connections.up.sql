-- Jira connections (TL-537): an org's Jira Cloud site, and the email and API token an admin imports
-- with, the token sealed (secrets.Box). It lasts 30 days from saving or extending, then it's deleted
-- (when read, and by the nightly purge).
CREATE TABLE jira_connections (
    org_id     text PRIMARY KEY,
    site       text NOT NULL,
    email      text NOT NULL,
    account    text NOT NULL DEFAULT '', -- the Jira user's name, to show whose token it is
    token      bytea NOT NULL,
    created_by text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL
);
