-- A backlog ticket can be closed in place (resolved) without going through a board. Resolved tickets
-- count as done everywhere (boards.DoneSQL) and leave the backlog, but stay readable by id.
ALTER TABLE tickets ADD COLUMN resolved_at TIMESTAMPTZ;
