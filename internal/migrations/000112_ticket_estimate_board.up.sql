-- TL-851: a backlog ticket can carry an estimate before it's planned (retro-board's poker). This is the
-- board whose scale it was given on, so planning or moving it onto a board with the same scale keeps it.
ALTER TABLE tickets_all ADD COLUMN estimate_board_id uuid REFERENCES boards (id) ON DELETE SET NULL;

-- tickets is a view over tickets_all (000058); recreate it so the new column shows
DROP VIEW tickets;
DO $$
DECLARE cols text;
BEGIN
    SELECT string_agg(quote_ident(attname), ', ' ORDER BY attnum) INTO cols
    FROM pg_attribute WHERE attrelid = 'tickets_all'::regclass AND attnum > 0 AND NOT attisdropped AND attname <> 'deleted_at';
    EXECUTE format('CREATE VIEW tickets AS SELECT %s FROM tickets_all WHERE deleted_at IS NULL', cols);
END $$;
