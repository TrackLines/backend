-- Projects, boards and roadmaps belong to a Clerk organization: owner_clerk_id now holds the
-- org's Clerk id (org_…) instead of a user's (user_…), so everyone in the org sees them.
ALTER TABLE projects DROP CONSTRAINT projects_owner_clerk_id_fkey;
ALTER TABLE boards   DROP CONSTRAINT boards_owner_clerk_id_fkey;
ALTER TABLE roadmaps DROP CONSTRAINT roadmaps_owner_clerk_id_fkey;

-- API keys stay owned by the person who made them, and act inside the org they were made in.
ALTER TABLE api_keys ADD COLUMN org_id TEXT;
CREATE INDEX api_keys_org_idx ON api_keys (org_id);

-- One-off: move the existing workspace into the chewedfeed org (a no-op on other databases).
UPDATE projects SET owner_clerk_id = 'org_3KKN50rj99aKwGW2beYtdKRdzQE' WHERE owner_clerk_id = 'user_3KKN3xhj4liKMVYSM1t47JD8IMw';
UPDATE boards   SET owner_clerk_id = 'org_3KKN50rj99aKwGW2beYtdKRdzQE' WHERE owner_clerk_id = 'user_3KKN3xhj4liKMVYSM1t47JD8IMw';
UPDATE roadmaps SET owner_clerk_id = 'org_3KKN50rj99aKwGW2beYtdKRdzQE' WHERE owner_clerk_id = 'user_3KKN3xhj4liKMVYSM1t47JD8IMw';
UPDATE api_keys SET org_id         = 'org_3KKN50rj99aKwGW2beYtdKRdzQE' WHERE owner_clerk_id = 'user_3KKN3xhj4liKMVYSM1t47JD8IMw';
