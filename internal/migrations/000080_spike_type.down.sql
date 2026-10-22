-- enum values can't be dropped; spikes become tasks
UPDATE tickets_all SET type = 'task' WHERE type = 'spike';
