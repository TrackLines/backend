-- Needs review: on boards with agent_review, an agent (API key) moving a ticket into Done lands it in
-- the column before Done with review_requested_at set, until a person approves or sends it back.
ALTER TABLE boards ADD COLUMN agent_review boolean NOT NULL DEFAULT false;
ALTER TABLE tickets ADD COLUMN review_requested_at timestamptz;
CREATE INDEX tickets_needs_review_idx ON tickets (board_id) WHERE review_requested_at IS NOT NULL;
