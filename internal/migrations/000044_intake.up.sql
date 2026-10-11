-- Request intake: a project's unlisted form link (NULL = closed) and the submissions waiting for triage.
ALTER TABLE projects ADD COLUMN intake_token text UNIQUE;
CREATE TABLE intake_submissions (
    id          uuid PRIMARY KEY DEFAULT uuidv7(),
    project_id  uuid NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    type        text NOT NULL CHECK (type IN ('bug', 'feature')),
    title       text NOT NULL,
    details     text NOT NULL DEFAULT '',
    contact     text NOT NULL DEFAULT '',        -- optional, e.g. an email to reply to; shown to the team only
    sender      text NOT NULL,                   -- hash of the sender's address, for rate limits only
    status      text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'promoted', 'dismissed')),
    ticket_id   uuid REFERENCES tickets (id) ON DELETE SET NULL,
    decided_by  text,
    decided_at  timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX intake_submissions_triage_idx ON intake_submissions (project_id, status, created_at DESC);
CREATE INDEX intake_submissions_sender_idx ON intake_submissions (sender, created_at);
