-- What a sprint held when it closed (before unfinished tickets moved on), so past burn charts
-- start from real scope. NULL for sprints closed before this existed.
ALTER TABLE sprints ADD COLUMN scope_tickets INTEGER, ADD COLUMN scope_points NUMERIC;
