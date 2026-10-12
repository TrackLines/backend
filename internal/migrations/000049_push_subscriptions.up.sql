-- Web Push: one row per person and device (browser); pushes go to every device a person turned on.
CREATE TABLE push_subscriptions (
    id         uuid PRIMARY KEY DEFAULT uuidv7(),
    user_id    text NOT NULL,
    endpoint   text NOT NULL UNIQUE,  -- the push service URL for this browser
    p256dh     text NOT NULL,
    auth       text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX push_subscriptions_user_idx ON push_subscriptions (user_id);
