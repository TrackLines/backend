-- A name starting with a digit ("2fa") gave a prefix like 2FA, which projects_key_prefix_format
-- rejects, so the project couldn't be created. Prefixes now always start with a letter (P2FA).
CREATE OR REPLACE FUNCTION project_key_prefix(project_name TEXT, org TEXT, self UUID) RETURNS TEXT AS $$
DECLARE
    base TEXT := left(regexp_replace(project_name, '[^A-Z]', '', 'g'), 6);
    candidate TEXT;
    n INT := 1;
BEGIN
    IF length(base) < 2 THEN
        base := upper(left(regexp_replace(project_name, '[^A-Za-z0-9]', '', 'g'), 3));
    END IF;
    IF base !~ '^[A-Z]' THEN
        base := 'P' || base;
    END IF;
    IF length(base) < 2 THEN
        base := base || 'X';
    END IF;
    candidate := base;
    WHILE EXISTS (SELECT 1 FROM projects WHERE owner_clerk_id = org AND key_prefix = candidate AND id IS DISTINCT FROM self) LOOP
        n := n + 1;
        candidate := left(base, 6 - length(n::text)) || n;
    END LOOP;
    RETURN candidate;
END $$ LANGUAGE plpgsql;
