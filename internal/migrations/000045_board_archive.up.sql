-- Boards that have been used are archived rather than deleted: off the project page, history kept.
ALTER TABLE boards ADD COLUMN archived_at timestamptz;
