-- Keep a record that an agent's work entered review after its active review request is cleared.
ALTER TABLE tickets ADD COLUMN agent_review_at timestamptz;
UPDATE tickets SET agent_review_at = review_requested_at WHERE review_requested_at IS NOT NULL;
