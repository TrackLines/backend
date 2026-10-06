-- Gantt bars need a start as well as the existing target date.
ALTER TABLE roadmap_items
    ADD COLUMN start_date DATE,
    ADD CONSTRAINT roadmap_items_start_before_target CHECK (start_date IS NULL OR target_date IS NULL OR start_date <= target_date);
