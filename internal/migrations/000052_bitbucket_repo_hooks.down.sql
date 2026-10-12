ALTER TABLE project_repos DROP COLUMN IF EXISTS webhook_secret;
ALTER TABLE project_repos DROP COLUMN IF EXISTS webhook_id;
ALTER TABLE project_repos DROP CONSTRAINT IF EXISTS project_repos_id_key;
ALTER TABLE project_repos DROP COLUMN IF EXISTS id;
