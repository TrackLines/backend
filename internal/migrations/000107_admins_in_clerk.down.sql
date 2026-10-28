CREATE OR REPLACE FUNCTION queue_org_admin() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        PERFORM audit_org(NEW.org_id, 'admin.granted', 'user', NEW.user_clerk_id, NULL, '{}', NEW.granted_by);
    ELSE
        PERFORM audit_org(OLD.org_id, 'admin.revoked', 'user', OLD.user_clerk_id, NULL, '{}');
    END IF;
    RETURN NULL;
END $$;
ALTER TABLE org_admins DROP COLUMN in_clerk;
