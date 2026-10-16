-- Google Chat app (TL-289): personal messages ("Ava assigned you TL-12") in the private chat each person
-- has with the Tracklines app. A person links their Google account once, from a link the app sends them.
CREATE TABLE chat_dms (
    user_clerk_id TEXT PRIMARY KEY,
    google_user   TEXT NOT NULL UNIQUE, -- users/123…: who the app talks to
    space         TEXT NOT NULL DEFAULT '', -- spaces/…: their chat with the app ('' = look it up)
    linked_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- one-time codes behind "Link your Tracklines account"; only the hash is kept
CREATE TABLE chat_link_codes (
    code_hash   TEXT PRIMARY KEY,
    google_user TEXT NOT NULL,
    space       TEXT NOT NULL DEFAULT '',
    expires_at  TIMESTAMPTZ NOT NULL
);
