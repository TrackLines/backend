-- When a ticket reached its board's Done column (last column); NULL while not done. Burn charts read it.
ALTER TABLE tickets ADD COLUMN done_at TIMESTAMPTZ;
-- best guess for tickets already done: their last change
UPDATE tickets t SET done_at = t.updated_at
WHERE t.column_id = (SELECT c.id FROM columns c WHERE c.board_id = t.board_id ORDER BY c.position DESC LIMIT 1);
