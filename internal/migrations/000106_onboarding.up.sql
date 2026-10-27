-- Setup belongs to the organization; completed guide versions belong to each person in it.
CREATE TABLE org_onboarding (
    org_id text PRIMARY KEY,
    step text NOT NULL DEFAULT 'project' CHECK (step IN ('project', 'invite', 'complete')),
    project_id uuid REFERENCES projects(id) ON DELETE SET NULL,
    board_id uuid REFERENCES boards(id) ON DELETE SET NULL,
    completed_at timestamptz,
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE guide_completions (
    org_id text NOT NULL,
    user_id text NOT NULL,
    guide_id text NOT NULL,
    version integer NOT NULL CHECK (version > 0),
    completed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, user_id, guide_id, version)
);
CREATE INDEX guide_completions_user ON guide_completions(user_id);
