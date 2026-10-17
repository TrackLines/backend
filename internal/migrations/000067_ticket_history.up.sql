-- Ticket lifecycle history (TL-74): an append-only record of each stage a ticket entered, or skipped,
-- from the backlog through its board's columns to resolved. Stage names are copied in, so history still
-- reads after columns are renamed, reordered or removed.
CREATE TABLE ticket_history (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY, -- also the order entries happened in
    ticket_id   UUID NOT NULL REFERENCES tickets_all (id) ON DELETE CASCADE, -- purged with the ticket
    project_id  UUID NOT NULL,
    at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    stage       TEXT NOT NULL CHECK (stage IN ('backlog', 'column', 'resolved')),
    outcome     TEXT NOT NULL CHECK (outcome IN ('entered', 'skipped')),
    stage_order INTEGER NOT NULL, -- backlog 0, a column its position + 1, resolved last
    board_id    UUID,             -- no foreign keys: the board or column may go, the record stays
    board_name  TEXT,
    column_id   UUID,
    column_name TEXT,
    done        BOOLEAN NOT NULL DEFAULT false, -- the board's last column (Done)
    resolution  TEXT,             -- for resolved: done, wont_do, duplicate
    actor       TEXT,             -- user id or agent name; null when the system did it
    CHECK ((stage = 'column') = (column_id IS NOT NULL)),
    CHECK ((stage = 'resolved') = (resolution IS NOT NULL)),
    CHECK (stage <> 'resolved' OR outcome = 'entered')
);
CREATE INDEX ticket_history_ticket_idx ON ticket_history (ticket_id, id);

-- append-only: no edits ever, and no deletes except when the ticket itself is purged (the cascade
-- runs inside the foreign key's own trigger, so it's one trigger level deeper than a direct delete)
CREATE FUNCTION ticket_history_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'UPDATE' OR pg_trigger_depth() < 2 THEN
        RAISE EXCEPTION 'ticket history is append-only' USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN OLD;
END $$;
CREATE TRIGGER ticket_history_append_only BEFORE UPDATE OR DELETE ON ticket_history
    FOR EACH ROW EXECUTE FUNCTION ticket_history_append_only();
