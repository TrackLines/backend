CREATE OR REPLACE FUNCTION tickets_assign_number() RETURNS trigger AS $$
BEGIN
    UPDATE projects SET ticket_seq = ticket_seq + 1 WHERE id = NEW.project_id RETURNING ticket_seq INTO NEW.number;
    RETURN NEW;
END $$ LANGUAGE plpgsql;
DROP TABLE import_items;
DROP TABLE imports;
