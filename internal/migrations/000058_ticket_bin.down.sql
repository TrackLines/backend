ALTER TABLE deleted_files DROP COLUMN deleted_on;
DROP VIEW tickets;
DELETE FROM tickets_all WHERE deleted_at IS NOT NULL;
DROP INDEX tickets_all_deleted_idx;
ALTER TABLE tickets_all DROP COLUMN deleted_at;
ALTER TABLE tickets_all RENAME TO tickets;
