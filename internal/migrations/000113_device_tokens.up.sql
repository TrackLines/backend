-- TL-358: the phone apps' push tokens (FCM on Android, and iOS through FCM too), one row per app install.
CREATE TABLE device_tokens (
    token       text PRIMARY KEY,
    user_id     text NOT NULL,
    platform    text NOT NULL CHECK (platform IN ('android', 'ios')),
    app_version text NOT NULL DEFAULT '',
    last_seen   timestamptz NOT NULL DEFAULT now(),
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX device_tokens_user_idx ON device_tokens (user_id);
