-- flags.gg ticket flags (TL-784): a ticket can need a feature flag, which follows it across the board.

-- the ticket's side: whether it needs one, its name (NULL: the key lower-cased), an agent override
-- (NULL: the project's default), the last error flags.gg gave, when the flag was made (locks the name),
-- and the "Remove flag" ticket made once it reached production (TL-788)
ALTER TABLE tickets_all
    ADD COLUMN needs_flag      boolean NOT NULL DEFAULT false,
    ADD COLUMN flag_name       text CHECK (flag_name IS NULL OR char_length(flag_name) BETWEEN 1 AND 100),
    ADD COLUMN flag_agent_id   text,
    ADD COLUMN flag_error      text,
    ADD COLUMN flag_created_at timestamptz,
    ADD COLUMN flag_cleanup_id uuid REFERENCES tickets_all (id) ON DELETE SET NULL;

-- tickets is a view over tickets_all (000058); recreate it so the new columns show
DROP VIEW tickets;
DO $$
DECLARE cols text;
BEGIN
    SELECT string_agg(quote_ident(attname), ', ' ORDER BY attnum) INTO cols
    FROM pg_attribute WHERE attrelid = 'tickets_all'::regclass AND attnum > 0 AND NOT attisdropped AND attname <> 'deleted_at';
    EXECUTE format('CREATE VIEW tickets AS SELECT %s FROM tickets_all WHERE deleted_at IS NULL', cols);
END $$;

-- what a column does to a ticket's flag when the ticket arrives; NULL: automatic (Testing/QA promote,
-- the board's dev complete column promotes to production)
ALTER TABLE columns ADD COLUMN flag_action text
    CHECK (flag_action IN ('none', 'promote', 'promote_to_production', 'archive'));

-- a project's default flags.gg agent, and how long after production its "Remove flag" tickets are due
ALTER TABLE projects
    ADD COLUMN flags_agent_id    text,
    ADD COLUMN flag_cleanup_days integer NOT NULL DEFAULT 30 CHECK (flag_cleanup_days BETWEEN 1 AND 365);

-- calls to flags.gg, queued in the transaction that made them, sent in order per ticket and retried
CREATE TABLE flag_outbox (
    id         bigserial PRIMARY KEY,
    org_id     text NOT NULL,
    ticket_id  uuid NOT NULL REFERENCES tickets_all (id) ON DELETE CASCADE,
    action     text NOT NULL CHECK (action IN ('create', 'promote', 'promote_to_production', 'archive')),
    attempts   integer NOT NULL DEFAULT 0,
    next_at    timestamptz NOT NULL DEFAULT now(),
    last_error text,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX flag_outbox_next ON flag_outbox (next_at, id);
CREATE INDEX flag_outbox_ticket ON flag_outbox (ticket_id, id);
