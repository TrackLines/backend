CREATE TABLE projects (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_clerk_id TEXT NOT NULL REFERENCES users (clerk_id) ON DELETE CASCADE,
    name           TEXT NOT NULL,
    description    TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- free-tier limit is SELECT count(*) ... WHERE owner_clerk_id = $1
CREATE INDEX projects_owner_idx ON projects (owner_clerk_id);

ALTER TABLE boards ADD COLUMN project_id UUID REFERENCES projects (id) ON DELETE CASCADE;
ALTER TABLE roadmaps ADD COLUMN project_id UUID REFERENCES projects (id) ON DELETE CASCADE;

-- backfill: one project per owner of existing boards/roadmaps
INSERT INTO projects (owner_clerk_id, name)
SELECT owner_clerk_id, 'My project' FROM boards
UNION
SELECT owner_clerk_id, 'My project' FROM roadmaps;

UPDATE boards b SET project_id = p.id FROM projects p WHERE p.owner_clerk_id = b.owner_clerk_id;
UPDATE roadmaps r SET project_id = p.id FROM projects p WHERE p.owner_clerk_id = r.owner_clerk_id;

ALTER TABLE boards ALTER COLUMN project_id SET NOT NULL;
ALTER TABLE roadmaps ALTER COLUMN project_id SET NOT NULL;
CREATE INDEX boards_project_idx ON boards (project_id);
CREATE INDEX roadmaps_project_idx ON roadmaps (project_id);
