DELETE FROM intake_submissions WHERE type = 'account_deletion';
ALTER TABLE intake_submissions DROP CONSTRAINT intake_submissions_type_check;
ALTER TABLE intake_submissions ADD CONSTRAINT intake_submissions_type_check CHECK (type IN ('bug', 'feature'));
