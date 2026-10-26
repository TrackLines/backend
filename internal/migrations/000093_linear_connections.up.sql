-- Linear connections (TL-654): the personal API key an admin imports a Linear workspace with, sealed
-- (secrets.Box). Like Jira's, it lasts 30 days from saving or extending, then it's deleted (when read,
-- and by the nightly purge).
CREATE TABLE linear_connections (
    org_id     text PRIMARY KEY,
    workspace  text NOT NULL,             -- the Linear workspace's name
    account    text NOT NULL DEFAULT '', -- whose key it is
    token      bytea NOT NULL,
    created_by text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL
);
