-- Deleted tickets go in a bin for 30 days before they're purged for good (TL-256). The table is
-- tickets_all; the tickets view is what's live, so every query that reads or writes "tickets" leaves
-- binned ones alone without having to remember to. Their comments, labels, links and attachments stay
-- with them until the purge.
--
-- NOTE for later migrations: change columns on tickets_all, then recreate the view (same list, minus
-- deleted_at). TestTicketsViewHasEveryColumn fails if the two drift apart.
ALTER TABLE tickets RENAME TO tickets_all;
ALTER TABLE tickets_all ADD COLUMN deleted_at timestamptz;
CREATE INDEX tickets_all_deleted_idx ON tickets_all (deleted_at) WHERE deleted_at IS NOT NULL;

DO $$
DECLARE cols text;
BEGIN
    SELECT string_agg(quote_ident(attname), ', ' ORDER BY attnum) INTO cols
    FROM pg_attribute WHERE attrelid = 'tickets_all'::regclass AND attnum > 0 AND NOT attisdropped AND attname <> 'deleted_at';
    EXECUTE format('CREATE VIEW tickets AS SELECT %s FROM tickets_all WHERE deleted_at IS NULL', cols);
END $$;

-- Deleted files wait in a dated batch (the day they were deleted: our "deleted folder", since
-- UploadThing has no folders), and the whole day goes from UploadThing 30 days after it.
ALTER TABLE deleted_files ADD COLUMN deleted_on date NOT NULL DEFAULT current_date;
UPDATE deleted_files SET deleted_on = deleted_at::date;
