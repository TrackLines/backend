-- Request forms also take account deletion requests (GDPR right to erasure).
ALTER TABLE intake_submissions DROP CONSTRAINT intake_submissions_type_check;
ALTER TABLE intake_submissions ADD CONSTRAINT intake_submissions_type_check CHECK (type IN ('bug', 'feature', 'account_deletion'));
