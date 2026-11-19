-- Used projects are archived rather than deleted: off the project lists, history kept (TL-907).
ALTER TABLE projects ADD COLUMN archived_at timestamptz;

CREATE OR REPLACE FUNCTION queue_project() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        PERFORM audit_org(NEW.owner_clerk_id, 'project.created', 'project', NEW.id::text, NEW.id, jsonb_build_object('name', NEW.name));
    ELSIF TG_OP = 'DELETE' THEN
        PERFORM audit_org(OLD.owner_clerk_id, 'project.deleted', 'project', OLD.id::text, NULL, jsonb_build_object('name', OLD.name));
    ELSE
        IF OLD.name IS DISTINCT FROM NEW.name THEN
            PERFORM audit_org(NEW.owner_clerk_id, 'project.renamed', 'project', NEW.id::text, NEW.id, audit_fromto(OLD.name, NEW.name));
        END IF;
        IF OLD.archived_at IS NULL AND NEW.archived_at IS NOT NULL THEN
            PERFORM audit_org(NEW.owner_clerk_id, 'project.archived', 'project', NEW.id::text, NEW.id, jsonb_build_object('name', NEW.name));
        ELSIF OLD.archived_at IS NOT NULL AND NEW.archived_at IS NULL THEN
            PERFORM audit_org(NEW.owner_clerk_id, 'project.restored', 'project', NEW.id::text, NEW.id, jsonb_build_object('name', NEW.name));
        END IF;
    END IF;
    RETURN NULL;
END $$;
DROP TRIGGER queue_project ON projects;
CREATE TRIGGER queue_project AFTER INSERT OR UPDATE OF name, archived_at OR DELETE ON projects FOR EACH ROW EXECUTE FUNCTION queue_project();
