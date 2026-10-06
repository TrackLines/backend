CREATE TYPE priority AS ENUM ('low', 'medium', 'high', 'urgent');
ALTER TABLE tickets ADD COLUMN priority priority NOT NULL DEFAULT 'medium';
