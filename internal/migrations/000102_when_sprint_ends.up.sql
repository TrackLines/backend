-- What a board does when a sprint reaches its end date (TL-279): start the next one straight away (as
-- until now), or end it and wait for a manager to start the next (as Jira does).
ALTER TABLE boards ADD COLUMN when_sprint_ends text NOT NULL DEFAULT 'start_next' CHECK (when_sprint_ends IN ('start_next', 'wait'));
