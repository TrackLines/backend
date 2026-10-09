-- An org's own board templates (built-in ones live in code: boards.Builtins). columns is
-- [{"name": "...", "wip_limit": n | null}, ...] in board order; the last is Done.
CREATE TABLE board_templates (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id         TEXT NOT NULL,
    name           TEXT NOT NULL,
    style          TEXT NOT NULL CHECK (style IN ('sprints', 'kanban')),
    estimate_scale TEXT NOT NULL CHECK (estimate_scale IN ('none', 'fibonacci', 'tshirt', 'powers', 'linear')),
    columns        JSONB NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX board_templates_org_idx ON board_templates (org_id);
