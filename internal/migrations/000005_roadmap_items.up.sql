CREATE TABLE roadmap_items (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    roadmap_id  UUID NOT NULL REFERENCES roadmaps (id) ON DELETE CASCADE,
    title       TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    target_date DATE,
    position    INTEGER NOT NULL
);
CREATE INDEX roadmap_items_roadmap_idx ON roadmap_items (roadmap_id, position);
