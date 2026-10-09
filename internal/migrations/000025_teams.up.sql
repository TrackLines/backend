CREATE TABLE teams (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id TEXT NOT NULL,
    name TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (id, org_id)
);
CREATE INDEX teams_org_idx ON teams (org_id);

CREATE TABLE team_members (
    team_id UUID NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    user_clerk_id TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (team_id, user_clerk_id)
);

ALTER TABLE projects ADD CONSTRAINT projects_id_owner_unique UNIQUE (id, owner_clerk_id);
CREATE TABLE project_teams (
    project_id UUID NOT NULL,
    team_id UUID NOT NULL,
    org_id TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, team_id),
    FOREIGN KEY (project_id, org_id) REFERENCES projects (id, owner_clerk_id) ON DELETE CASCADE,
    FOREIGN KEY (team_id, org_id) REFERENCES teams (id, org_id) ON DELETE CASCADE
);
CREATE INDEX project_teams_team_idx ON project_teams (team_id);
