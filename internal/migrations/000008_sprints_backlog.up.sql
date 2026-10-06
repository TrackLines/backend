CREATE TYPE ticket_type AS ENUM ('bug', 'feature', 'task');

-- A sprint is a time box on one team board; at most one open sprint per board.
CREATE TABLE sprints (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    board_id    UUID NOT NULL REFERENCES boards (id) ON DELETE CASCADE,
    number      INTEGER NOT NULL,
    length_days INTEGER NOT NULL CHECK (length_days BETWEEN 1 AND 365),
    starts_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    ends_at     TIMESTAMPTZ NOT NULL,
    closed_at   TIMESTAMPTZ,
    UNIQUE (board_id, number)
);
CREATE UNIQUE INDEX sprints_one_open_per_board ON sprints (board_id) WHERE closed_at IS NULL;
CREATE INDEX sprints_open_due_idx ON sprints (ends_at) WHERE closed_at IS NULL; -- auto-close scan

-- Tickets always belong to a project; board+column are both NULL for backlog tickets.
ALTER TABLE tickets
    ADD COLUMN type ticket_type NOT NULL DEFAULT 'task',
    ADD COLUMN project_id UUID REFERENCES projects (id) ON DELETE CASCADE,
    ADD COLUMN sprint_id UUID REFERENCES sprints (id) ON DELETE SET NULL;
UPDATE tickets t SET project_id = b.project_id FROM boards b WHERE b.id = t.board_id;
ALTER TABLE tickets
    ALTER COLUMN project_id SET NOT NULL,
    ALTER COLUMN board_id DROP NOT NULL,
    ALTER COLUMN column_id DROP NOT NULL,
    ADD CONSTRAINT tickets_board_and_column_together CHECK ((board_id IS NULL) = (column_id IS NULL));
CREATE INDEX tickets_backlog_idx ON tickets (project_id, position) WHERE board_id IS NULL;
CREATE INDEX tickets_sprint_idx ON tickets (sprint_id);
