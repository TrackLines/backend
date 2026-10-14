-- Files the year-old clean-up deleted (TL-260), so org admins can see what went after the ticket,
-- or the file, is gone. Only what's needed to say what it was: the file itself is deleted.
CREATE TABLE expired_attachments (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id       text NOT NULL,
    ticket_id    uuid NOT NULL,
    ticket_key   text NOT NULL,
    ticket_title text NOT NULL,
    name         text NOT NULL,
    size_bytes   bigint NOT NULL,
    created_by   text NOT NULL,
    deleted_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX expired_attachments_org_idx ON expired_attachments (org_id, deleted_at DESC);
