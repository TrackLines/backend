-- Pull requests linked to tickets: a PR in a project's linked repo that mentions a ticket key
-- (TL-123) in its branch, title or description. Kept up to date by the provider webhooks.
CREATE TABLE ticket_prs (
    id         UUID PRIMARY KEY DEFAULT uuidv7(),
    ticket_id  UUID NOT NULL REFERENCES tickets (id) ON DELETE CASCADE,
    provider   TEXT NOT NULL CHECK (provider IN ('github', 'bitbucket')),
    repo       TEXT NOT NULL,  -- owner/repo
    number     INTEGER NOT NULL,
    url        TEXT NOT NULL,
    title      TEXT NOT NULL DEFAULT '',
    state      TEXT NOT NULL CHECK (state IN ('open', 'draft', 'merged', 'closed')),
    author     TEXT NOT NULL DEFAULT '',
    branch     TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (ticket_id, provider, repo, number)
);
CREATE INDEX ticket_prs_pr_idx ON ticket_prs (provider, repo, number);
