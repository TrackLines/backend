ALTER TABLE tickets ADD COLUMN duplicate_of UUID REFERENCES tickets (id) ON DELETE SET NULL;
CREATE INDEX tickets_duplicate_of_idx ON tickets (duplicate_of) WHERE duplicate_of IS NOT NULL;

ALTER TABLE tickets DROP CONSTRAINT tickets_resolution_check;
ALTER TABLE tickets ADD CONSTRAINT tickets_resolution_check CHECK (resolution IN ('done', 'wont_do', 'duplicate'));
