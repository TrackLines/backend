CREATE TABLE boards (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_clerk_id TEXT NOT NULL REFERENCES users (clerk_id) ON DELETE CASCADE,
    name           TEXT NOT NULL,
    description    TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- free-tier limit is SELECT count(*) ... WHERE owner_clerk_id = $1 (served by this index)
CREATE INDEX boards_owner_idx ON boards (owner_clerk_id);

CREATE TABLE columns (
    id       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    board_id UUID NOT NULL REFERENCES boards (id) ON DELETE CASCADE,
    name     TEXT NOT NULL,
    position INTEGER NOT NULL
);
CREATE INDEX columns_board_idx ON columns (board_id, position);
