DROP TRIGGER tickets_number ON tickets;
DROP FUNCTION tickets_assign_number();
ALTER TABLE tickets DROP COLUMN number;
DROP TRIGGER projects_key_prefix ON projects;
DROP FUNCTION projects_default_key_prefix();
ALTER TABLE projects DROP COLUMN key_prefix, DROP COLUMN ticket_seq;
DROP FUNCTION project_key_prefix(TEXT, TEXT, UUID);
