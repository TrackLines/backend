-- The column agents' finished work waits in for a person's review (TL-592), named rather than guessed
-- as "the column before Done". Null: the column before Done, as before.
ALTER TABLE boards ADD COLUMN review_column_id uuid REFERENCES columns (id) ON DELETE SET NULL;
