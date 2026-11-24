-- "Latest tickets" on the home Mentions card, per user: how many (0 = off, else 1–20, paged 5 at a
-- time) and at most how many from any one project.
ALTER TABLE users ADD COLUMN latest_tickets smallint NOT NULL DEFAULT 0 CHECK (latest_tickets BETWEEN 0 AND 20),
    ADD COLUMN latest_per_project smallint NOT NULL DEFAULT 3 CHECK (latest_per_project BETWEEN 1 AND 20);
