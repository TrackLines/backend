-- The first time a ticket leaves its board's first column. Historical start times can't be
-- reconstructed accurately, so existing tickets remain NULL.
ALTER TABLE tickets ADD COLUMN started_at TIMESTAMPTZ;
