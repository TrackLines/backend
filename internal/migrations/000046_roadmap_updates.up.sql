CREATE TABLE roadmap_updates (
    id           uuid PRIMARY KEY DEFAULT uuidv7(),
    roadmap_id   uuid NOT NULL REFERENCES roadmaps (id) ON DELETE CASCADE,
    health       text NOT NULL CHECK (health IN ('on_track', 'at_risk', 'off_track')),
    note         text NOT NULL CHECK (char_length(note) BETWEEN 1 AND 2000),
    published_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX roadmap_updates_history_idx ON roadmap_updates (roadmap_id, published_at, id);
