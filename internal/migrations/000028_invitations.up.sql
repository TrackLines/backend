CREATE TABLE invitations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id TEXT NOT NULL,
    clerk_invitation_id TEXT NOT NULL UNIQUE,
    email_address TEXT NOT NULL,
    scope TEXT NOT NULL CHECK (scope IN ('organization', 'team', 'project')),
    target_id UUID,
    invited_by TEXT NOT NULL,
    accept_url TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'accepted', 'revoked')),
    accepted_user_clerk_id TEXT,
    accepted_at TIMESTAMPTZ,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((scope = 'organization' AND target_id IS NULL) OR (scope <> 'organization' AND target_id IS NOT NULL))
);
CREATE INDEX invitations_org_idx ON invitations (org_id, created_at DESC);
CREATE INDEX invitations_pending_email_idx ON invitations (org_id, lower(email_address)) WHERE status = 'pending';

-- Project membership is tracked for project invitations. Organization members retain org-wide read access.
CREATE TABLE project_members (
    project_id UUID NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    user_clerk_id TEXT NOT NULL,
    invited_by TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, user_clerk_id)
);
CREATE INDEX project_members_user_idx ON project_members (user_clerk_id);
