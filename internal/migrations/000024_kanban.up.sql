-- Boards run in sprints (time boxes) or as kanban (continuous flow with work-in-progress limits).
ALTER TABLE boards ADD COLUMN style TEXT NOT NULL DEFAULT 'sprints' CHECK (style IN ('sprints', 'kanban'));
-- Advisory: the board warns when a column holds more tickets than this; moves aren't refused.
ALTER TABLE columns ADD COLUMN wip_limit INTEGER CHECK (wip_limit > 0);
