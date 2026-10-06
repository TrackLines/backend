DELETE FROM tickets WHERE board_id IS NULL; -- backlog tickets can't exist without a board
ALTER TABLE tickets
    DROP CONSTRAINT tickets_board_and_column_together,
    ALTER COLUMN board_id SET NOT NULL,
    ALTER COLUMN column_id SET NOT NULL,
    DROP COLUMN sprint_id,
    DROP COLUMN project_id,
    DROP COLUMN type;
DROP TABLE sprints;
DROP TYPE ticket_type;
