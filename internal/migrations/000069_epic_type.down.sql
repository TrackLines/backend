-- enum values can't be dropped; epics become features
UPDATE tickets_all SET type = 'feature' WHERE type = 'epic';
