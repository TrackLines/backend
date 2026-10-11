-- In-app notifications (internal; external channels such as notfy.dev, email and chat come later).
CREATE TABLE notifications (
    id         uuid PRIMARY KEY DEFAULT uuidv7(),
    org_id     text NOT NULL,
    user_id    text NOT NULL,                -- the Clerk user told
    kind       text NOT NULL,                -- assigned | mention | finished | wont_do | review | approval | sent_back
    text       text NOT NULL,
    actor      text NOT NULL DEFAULT '',      -- who did it (user id or agent name)
    ticket_id  uuid REFERENCES tickets (id) ON DELETE CASCADE,
    board_id   uuid REFERENCES boards (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    read_at    timestamptz
);
CREATE INDEX notifications_inbox_idx ON notifications (user_id, org_id, created_at DESC);
CREATE INDEX notifications_unread_idx ON notifications (user_id, org_id) WHERE read_at IS NULL;
