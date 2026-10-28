-- TL-690: an org admin is Clerk's org:admin role. org_admins mirrors it (written by the admin API
-- and the Clerk webhook) so checks never wait on Clerk. Rows from before this release are pushed to
-- Clerk once (Admins.SyncToClerk): in_clerk marks the ones done; every later row starts true.
ALTER TABLE org_admins ADD COLUMN in_clerk boolean NOT NULL DEFAULT false;
ALTER TABLE org_admins ALTER COLUMN in_clerk SET DEFAULT true;

-- granted_by is only the actor when it's a person (an admin granting); Clerk (its dashboard, SSO) is the system.
CREATE OR REPLACE FUNCTION queue_org_admin() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        PERFORM audit_org(NEW.org_id, 'admin.granted', 'user', NEW.user_clerk_id, NULL, '{}',
            CASE WHEN NEW.granted_by LIKE 'user\_%' THEN NEW.granted_by END);
    ELSE
        PERFORM audit_org(OLD.org_id, 'admin.revoked', 'user', OLD.user_clerk_id, NULL, '{}');
    END IF;
    RETURN NULL;
END $$;
