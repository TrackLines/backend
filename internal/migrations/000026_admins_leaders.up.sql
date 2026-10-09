-- TrackLines admins per Clerk organization. The first one is resolved from Clerk (see
-- organizations.Admins.seed); granted_by records how: clerk:creator, clerk:admin-role,
-- clerk:earliest-member, or the user id of the admin who granted it.
CREATE TABLE org_admins (
    org_id        TEXT NOT NULL,
    user_clerk_id TEXT NOT NULL,
    granted_by    TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, user_clerk_id)
);

-- A team leader is a team member with the flag; leaving the team ends the leadership.
ALTER TABLE team_members ADD COLUMN leader BOOLEAN NOT NULL DEFAULT false;

-- The team that runs a board; its leaders' authority is scoped to these boards.
-- Same org and linked to the board's project, checked by the app (teams.SetBoardTeam).
ALTER TABLE boards ADD COLUMN team_id UUID REFERENCES teams (id) ON DELETE SET NULL;
CREATE INDEX boards_team_idx ON boards (team_id) WHERE team_id IS NOT NULL;
