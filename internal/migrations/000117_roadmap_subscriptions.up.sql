-- TL-902: people who asked for a public roadmap's updates by email. token: the secret in an address's
-- confirm and unsubscribe links. No endpoint returns an address. verified_at null: not confirmed yet,
-- nothing is sent. sent_at: when the confirmation email last went, so a repeat request can't flood an inbox.
CREATE TABLE roadmap_subscriptions (
    id          uuid PRIMARY KEY DEFAULT uuidv7(),
    roadmap_id  uuid NOT NULL REFERENCES roadmaps (id) ON DELETE CASCADE,
    email       text NOT NULL CHECK (email = lower(email) AND char_length(email) BETWEEN 3 AND 254),
    token       text NOT NULL UNIQUE,
    verified_at timestamptz,
    sent_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (roadmap_id, email)
);
