-- Who was @mentioned on which ticket, for "tickets that mention me". Kept apart from notifications,
-- which people clear.
CREATE TABLE ticket_mentions (
    id         uuid PRIMARY KEY DEFAULT uuidv7(),
    org_id     text NOT NULL,
    user_id    text NOT NULL,                -- the Clerk user mentioned
    ticket_id  uuid NOT NULL REFERENCES tickets (id) ON DELETE CASCADE,
    comment_id uuid REFERENCES ticket_comments (id) ON DELETE CASCADE, -- null: backfilled
    actor      text NOT NULL DEFAULT '',      -- who mentioned them
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ticket_mentions_mine_idx ON ticket_mentions (user_id, org_id, created_at DESC);

-- mentions from before this table, from the notifications nobody has cleared yet
INSERT INTO ticket_mentions (org_id, user_id, ticket_id, actor, created_at)
SELECT org_id, user_id, ticket_id, actor, created_at FROM notifications WHERE kind = 'mention' AND ticket_id IS NOT NULL;
