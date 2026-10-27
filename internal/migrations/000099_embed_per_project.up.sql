-- Embedding belongs to the project (TL-683): roadmaps and request forms are made there, so the sites that
-- may show them, and the embed keys, are the project's. Deleting a project takes them with it.
ALTER TABLE embed_origins DROP CONSTRAINT embed_origins_org_origin_key;
ALTER TABLE embed_origins ADD COLUMN project_id uuid REFERENCES projects (id) ON DELETE CASCADE;
-- what an org allowed until now, every one of its projects allows, so nothing that embeds today breaks
INSERT INTO embed_origins (org, origin, created_by, created_at, project_id)
SELECT o.org, o.origin, o.created_by, o.created_at, p.id
FROM embed_origins o JOIN projects p ON p.owner_clerk_id = o.org
WHERE o.project_id IS NULL;
DELETE FROM embed_origins WHERE project_id IS NULL;
ALTER TABLE embed_origins ALTER COLUMN project_id SET NOT NULL;
ALTER TABLE embed_origins ADD CONSTRAINT embed_origins_project_origin_key UNIQUE (project_id, origin);
DROP INDEX embed_origins_org_idx;

-- a key's token is already in snippets, so it can't be copied: it goes to the org's oldest project
-- (pre-launch, behind the embedding flag); an org with no projects has nothing to embed, so its keys go
ALTER TABLE embed_keys ADD COLUMN project_id uuid REFERENCES projects (id) ON DELETE CASCADE;
UPDATE embed_keys k SET project_id = (SELECT p.id FROM projects p WHERE p.owner_clerk_id = k.org ORDER BY p.created_at, p.id LIMIT 1);
DELETE FROM embed_keys WHERE project_id IS NULL;
ALTER TABLE embed_keys ALTER COLUMN project_id SET NOT NULL;
CREATE INDEX embed_keys_project_idx ON embed_keys (project_id);
DROP INDEX embed_keys_org_idx;
