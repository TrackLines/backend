-- Conversation on a ticket, kept apart from ticket.description. parent_comment_id makes replies;
-- the composite FK guarantees a reply's parent is on the same ticket.
CREATE TABLE ticket_comments (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    ticket_id         UUID NOT NULL REFERENCES tickets (id) ON DELETE CASCADE,
    parent_comment_id UUID,
    body              TEXT NOT NULL CHECK (length(btrim(body)) > 0),
    author            TEXT NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (ticket_id, id),
    FOREIGN KEY (ticket_id, parent_comment_id) REFERENCES ticket_comments (ticket_id, id) ON DELETE CASCADE
);
CREATE INDEX ticket_comments_ticket_idx ON ticket_comments (ticket_id, created_at);
