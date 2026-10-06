ALTER TABLE roadmap_items
    DROP CONSTRAINT roadmap_items_manual_status_check,
    DROP COLUMN manual_status;
