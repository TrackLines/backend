-- Saved views (TL-633): a named set of backlog/search filters for a project. Personal (only its owner
-- sees it) or shared with everyone in the project (made by admins and the project's team leaders).
CREATE TABLE saved_views (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id     text NOT NULL,
    project_id UUID NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    owner      text NOT NULL, -- who made it (a user id)
    name       text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 80),
    shared     boolean NOT NULL DEFAULT false,
    query      jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(query) = 'object'),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX saved_views_project_idx ON saved_views (project_id, owner);
