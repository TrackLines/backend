DROP INDEX IF EXISTS tickets_needs_review_idx;
ALTER TABLE tickets DROP COLUMN IF EXISTS review_requested_at;
ALTER TABLE boards DROP COLUMN IF EXISTS agent_review;
