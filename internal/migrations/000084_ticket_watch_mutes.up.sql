-- Watching (TL-587): everyone @mentioned on a ticket hears what happens to it, unless they muted it here.
-- The mention stays on their Mentions list either way, so unmuting needs no new mention.
CREATE TABLE ticket_watch_mutes (
    org_id     text NOT NULL,
    user_id    text NOT NULL,
    ticket_id  uuid NOT NULL REFERENCES tickets_all (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, ticket_id)
);
