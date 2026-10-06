ALTER TABLE tickets
    ADD COLUMN created_by TEXT NOT NULL DEFAULT 'legacy',
    ADD COLUMN assigned_to TEXT;
