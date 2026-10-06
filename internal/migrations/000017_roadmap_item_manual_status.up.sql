ALTER TABLE roadmap_items
    ADD COLUMN manual_status TEXT NOT NULL DEFAULT 'not_started',
    ADD CONSTRAINT roadmap_items_manual_status_check
        CHECK (manual_status IN ('not_started', 'in_progress', 'done'));
