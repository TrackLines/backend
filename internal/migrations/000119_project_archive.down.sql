DROP TRIGGER queue_project ON projects;
CREATE OR REPLACE FUNCTION queue_project() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        PERFORM audit_org(NEW.owner_clerk_id, 'project.created', 'project', NEW.id::text, NEW.id, jsonb_build_object('name', NEW.name));
    ELSIF TG_OP = 'DELETE' THEN
        PERFORM audit_org(OLD.owner_clerk_id, 'project.deleted', 'project', OLD.id::text, NULL, jsonb_build_object('name', OLD.name));
    ELSIF OLD.name IS DISTINCT FROM NEW.name THEN
        PERFORM audit_org(NEW.owner_clerk_id, 'project.renamed', 'project', NEW.id::text, NEW.id, audit_fromto(OLD.name, NEW.name));
    END IF;
    RETURN NULL;
END $$;
CREATE TRIGGER queue_project AFTER INSERT OR UPDATE OF name OR DELETE ON projects FOR EACH ROW EXECUTE FUNCTION queue_project();
ALTER TABLE projects DROP COLUMN archived_at;
