ALTER TABLE tickets DROP CONSTRAINT tickets_planned_in_backlog, DROP COLUMN planned_sprint_id;
DROP TABLE planned_sprints;
