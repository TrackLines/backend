-- One key per agent; acts as its owner. Only the sha256 of the key is stored.
CREATE TABLE api_keys (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_clerk_id TEXT NOT NULL REFERENCES users (clerk_id) ON DELETE CASCADE,
    name           TEXT NOT NULL,
    prefix         TEXT NOT NULL, -- first chars of the key, so people can tell keys apart
    hash           BYTEA NOT NULL UNIQUE,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at   TIMESTAMPTZ,
    revoked_at     TIMESTAMPTZ
);
CREATE INDEX api_keys_owner_idx ON api_keys (owner_clerk_id);
