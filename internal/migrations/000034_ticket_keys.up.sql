-- Ticket keys (TL-123): a per-project prefix plus a per-project number, assigned by the database
-- on insert so every path (API, bugfixes, MCP, tests) gets one. Keys go in branch names, PR titles
-- and search.

-- A prefix from the name: its capitals (TrackLines → TL), else its first letters (flags → FLA);
-- 2–6 characters, made unique within the org by a trailing number (TL, TL2, …).
CREATE FUNCTION project_key_prefix(project_name TEXT, org TEXT, self UUID) RETURNS TEXT AS $$
DECLARE
    base TEXT := left(regexp_replace(project_name, '[^A-Z]', '', 'g'), 6);
    candidate TEXT;
    n INT := 1;
BEGIN
    IF length(base) < 2 THEN
        base := upper(left(regexp_replace(project_name, '[^A-Za-z0-9]', '', 'g'), 3));
    END IF;
    IF length(base) < 2 THEN
        base := 'P' || base || 'X';
    END IF;
    candidate := base;
    WHILE EXISTS (SELECT 1 FROM projects WHERE owner_clerk_id = org AND key_prefix = candidate AND id IS DISTINCT FROM self) LOOP
        n := n + 1;
        candidate := left(base, 6 - length(n::text)) || n;
    END LOOP;
    RETURN candidate;
END $$ LANGUAGE plpgsql;

ALTER TABLE projects ADD COLUMN key_prefix TEXT, ADD COLUMN ticket_seq INTEGER NOT NULL DEFAULT 0;
DO $$
DECLARE p RECORD;
BEGIN
    FOR p IN SELECT id, name, owner_clerk_id FROM projects ORDER BY created_at LOOP
        UPDATE projects SET key_prefix = project_key_prefix(p.name, p.owner_clerk_id, p.id) WHERE id = p.id;
    END LOOP;
END $$;
ALTER TABLE projects ALTER COLUMN key_prefix SET NOT NULL,
    ADD CONSTRAINT projects_key_prefix_format CHECK (key_prefix ~ '^[A-Z][A-Z0-9]{1,5}$'),
    ADD CONSTRAINT projects_key_prefix_unique UNIQUE (owner_clerk_id, key_prefix);

CREATE FUNCTION projects_default_key_prefix() RETURNS trigger AS $$
BEGIN
    IF NEW.key_prefix IS NULL THEN
        NEW.key_prefix := project_key_prefix(NEW.name, NEW.owner_clerk_id, NEW.id);
    END IF;
    RETURN NEW;
END $$ LANGUAGE plpgsql;
CREATE TRIGGER projects_key_prefix BEFORE INSERT ON projects FOR EACH ROW EXECUTE FUNCTION projects_default_key_prefix();

-- numbers: existing tickets in creation order, new ones from the project's counter (the row lock
-- on the project serialises concurrent creates)
ALTER TABLE tickets ADD COLUMN number INTEGER;
UPDATE tickets t SET number = n.num FROM (
    SELECT id, row_number() OVER (PARTITION BY project_id ORDER BY created_at, id) AS num FROM tickets) n
WHERE n.id = t.id;
UPDATE projects p SET ticket_seq = COALESCE((SELECT max(number) FROM tickets WHERE project_id = p.id), 0);
ALTER TABLE tickets ALTER COLUMN number SET NOT NULL, ADD CONSTRAINT tickets_project_number_unique UNIQUE (project_id, number);

CREATE FUNCTION tickets_assign_number() RETURNS trigger AS $$
BEGIN
    UPDATE projects SET ticket_seq = ticket_seq + 1 WHERE id = NEW.project_id RETURNING ticket_seq INTO NEW.number;
    RETURN NEW;
END $$ LANGUAGE plpgsql;
CREATE TRIGGER tickets_number BEFORE INSERT ON tickets FOR EACH ROW EXECUTE FUNCTION tickets_assign_number();
