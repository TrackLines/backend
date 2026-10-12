-- Bitbucket webhooks are made per linked repo by Tracklines itself, each with its own secret
-- (sealed, bound to the link) and URL (/api/webhooks/bitbucket/{id}), instead of one shared secret (TL-165).
ALTER TABLE project_repos ADD COLUMN id uuid NOT NULL DEFAULT uuidv7();
ALTER TABLE project_repos ADD CONSTRAINT project_repos_id_key UNIQUE (id);
ALTER TABLE project_repos ADD COLUMN webhook_id text;      -- Bitbucket's hook uuid
ALTER TABLE project_repos ADD COLUMN webhook_secret bytea;  -- sealed; never shown
