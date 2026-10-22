-- Embed usage keys (TL-497/498): a public identifier an org puts in its widget snippet (data-key), so
-- embed traffic can be attributed for usage stats. Not a secret and does not gate access — the origin
-- allow-list (embed_origins) remains the security boundary.
CREATE TABLE embed_keys (
    id         uuid PRIMARY KEY DEFAULT uuidv7(),
    org        text NOT NULL,                 -- the owning organisation (Clerk org id)
    key        text NOT NULL UNIQUE,          -- public token, tle_...
    label      text NOT NULL DEFAULT '',      -- what the org calls this embed/placement
    created_by text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX embed_keys_org_idx ON embed_keys (org);

-- One counter row per key/origin/resource/day: a widget hit bumps hits.
CREATE TABLE embed_usage (
    embed_key_id uuid NOT NULL REFERENCES embed_keys (id) ON DELETE CASCADE,
    origin       text NOT NULL,
    resource     text NOT NULL,               -- 'roadmap' | 'intake'
    day          date NOT NULL,
    hits         bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (embed_key_id, origin, resource, day)
);
