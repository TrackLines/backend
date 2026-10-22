-- A spike's timebox (TL-502): days + hours (hours 0–23), stored as entered — no working-day
-- conversion. Null on non-spikes and on spikes not yet timed. Set via PATCH /tickets/{id}.
ALTER TABLE tickets_all
    ADD COLUMN timebox_days  smallint,
    ADD COLUMN timebox_hours smallint;

-- tickets is a view over tickets_all (000058); recreate it so the new columns show (same list, minus
-- deleted_at). TestTicketsViewHasEveryColumn fails if they drift apart.
DROP VIEW tickets;
DO $$
DECLARE cols text;
BEGIN
    SELECT string_agg(quote_ident(attname), ', ' ORDER BY attnum) INTO cols
    FROM pg_attribute WHERE attrelid = 'tickets_all'::regclass AND attnum > 0 AND NOT attisdropped AND attname <> 'deleted_at';
    EXECUTE format('CREATE VIEW tickets AS SELECT %s FROM tickets_all WHERE deleted_at IS NULL', cols);
END $$;
