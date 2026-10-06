-- 000017_sub_tickets.down.sql — T-033: revert sub-tickets
ALTER TABLE tickets DROP COLUMN IF EXISTS parent_id;
DROP INDEX IF EXISTS tickets_parent_idx;
