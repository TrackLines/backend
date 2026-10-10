-- Refinement: up to two planned sprints per board, after its current sprint. Backlog tickets are
-- planned into one (and sized on that board's scale) while they stay in the backlog. Closing the
-- current sprint promotes planned sprint 1, unless it's over capacity without approval.
CREATE TABLE planned_sprints (
    id             UUID PRIMARY KEY DEFAULT uuidv7(),
    board_id       UUID NOT NULL REFERENCES boards (id) ON DELETE CASCADE,
    position       SMALLINT NOT NULL CHECK (position IN (1, 2)), -- 1 = next sprint
    length_days    INTEGER NOT NULL CHECK (length_days BETWEEN 1 AND 365),
    approved_total NUMERIC,     -- the planned total an admin/team leader accepted over capacity
    approved_by    TEXT,
    approved_at    TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (board_id, position) DEFERRABLE INITIALLY DEFERRED
);

ALTER TABLE tickets
    ADD COLUMN planned_sprint_id UUID REFERENCES planned_sprints (id) ON DELETE SET NULL,
    ADD CONSTRAINT tickets_planned_in_backlog CHECK (planned_sprint_id IS NULL OR board_id IS NULL);
CREATE INDEX tickets_planned_sprint_idx ON tickets (planned_sprint_id) WHERE planned_sprint_id IS NOT NULL;
