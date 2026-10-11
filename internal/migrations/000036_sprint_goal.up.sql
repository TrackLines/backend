-- A one-line sprint goal: set when planning or starting, shown on the sprint bar and in standup.
ALTER TABLE sprints ADD COLUMN goal TEXT NOT NULL DEFAULT '' CHECK (length(goal) <= 200);
ALTER TABLE planned_sprints ADD COLUMN goal TEXT NOT NULL DEFAULT '' CHECK (length(goal) <= 200);
