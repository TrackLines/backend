CREATE TYPE roadmap_visibility AS ENUM ('public', 'login_only', 'team');

CREATE TABLE roadmaps (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_clerk_id TEXT NOT NULL REFERENCES users (clerk_id) ON DELETE CASCADE,
    title          TEXT NOT NULL,
    description    TEXT NOT NULL DEFAULT '',
    visibility     roadmap_visibility NOT NULL DEFAULT 'public',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX roadmaps_owner_idx ON roadmaps (owner_clerk_id);
CREATE INDEX roadmaps_public_idx ON roadmaps (updated_at DESC) WHERE visibility = 'public';
