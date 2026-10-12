UPDATE tickets SET resolution = 'done' WHERE resolution = 'duplicate';
ALTER TABLE tickets DROP CONSTRAINT tickets_resolution_check;
ALTER TABLE tickets ADD CONSTRAINT tickets_resolution_check CHECK (resolution IN ('done', 'wont_do'));
DROP INDEX tickets_duplicate_of_idx;
ALTER TABLE tickets DROP COLUMN duplicate_of;
