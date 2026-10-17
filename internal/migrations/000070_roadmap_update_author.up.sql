-- who posted a roadmap update (TL-413): their id, and their name as it was when they posted, so public
-- roadmaps can say "posted by Ava" without looking anyone up. Older updates have none.
ALTER TABLE roadmap_updates ADD COLUMN author_id text, ADD COLUMN author_name text;
