-- A ticket's due date (TL-631): a day, no time; "overdue" is decided in the caller's local day.
ALTER TABLE tickets_all ADD COLUMN due_date date;

-- tickets is a view over tickets_all (000058); recreate it so the new column shows (same list, minus
-- deleted_at). TestTicketsViewHasEveryColumn fails if they drift apart.
DROP VIEW tickets;
DO $$
DECLARE cols text;
BEGIN
    SELECT string_agg(quote_ident(attname), ', ' ORDER BY attnum) INTO cols
    FROM pg_attribute WHERE attrelid = 'tickets_all'::regclass AND attnum > 0 AND NOT attisdropped AND attname <> 'deleted_at';
    EXECUTE format('CREATE VIEW tickets AS SELECT %s FROM tickets_all WHERE deleted_at IS NULL', cols);
END $$;

-- Due-date reminders already sent (the day before, and on the day), so each goes once.
CREATE TABLE due_reminders (
    ticket_id UUID NOT NULL REFERENCES tickets_all (id) ON DELETE CASCADE,
    due_date  date NOT NULL,
    kind      text NOT NULL CHECK (kind IN ('tomorrow', 'today')),
    sent_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (ticket_id, due_date, kind)
);
