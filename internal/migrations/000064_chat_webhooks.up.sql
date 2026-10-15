-- Google Chat (TL-288): notifications posted into a Chat space through its incoming webhook, one
-- per project and one for the whole org (project_id NULL). The webhook URL carries its own key, so
-- it's sealed like other integration secrets; kinds are which notifications it gets.
CREATE TABLE chat_webhooks (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id     text NOT NULL,
    project_id uuid REFERENCES projects (id) ON DELETE CASCADE,
    url        bytea NOT NULL,
    kinds      text[] NOT NULL,
    updated_by text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX chat_webhooks_scope ON chat_webhooks (org_id, coalesce(project_id::text, ''));
