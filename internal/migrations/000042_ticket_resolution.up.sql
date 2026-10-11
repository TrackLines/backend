-- How a resolved backlog ticket was closed: NULL/done = finished, wont_do = decided against (it stays,
-- readable, so whoever raised it can be told). Cleared when the ticket is reopened onto a board.
ALTER TABLE tickets ADD COLUMN resolution text CHECK (resolution IN ('done', 'wont_do'));
