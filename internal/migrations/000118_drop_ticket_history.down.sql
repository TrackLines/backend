-- Puts the table and its triggers back, empty: the history itself stays in the auditor (re-running
-- migration 000097's backfill can't bring it back). Import fingerprints aren't recomputed.
DROP TRIGGER record_ticket_history ON tickets_all;
DROP FUNCTION record_ticket_history();
DROP FUNCTION audit_ticket_stage(tickets_all, text, text, integer, text, uuid, text, boolean, text);

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

-- Record ticket lifecycle history (TL-75) from the tickets table itself, so every way a ticket can move
-- (API, MCP, integrations, sprint promotion, bulk updates) is covered, in the same transaction as the
-- change. Who did it: the transaction's tracklines.actor (set by the app), or for a new ticket its creator.
--   created in the backlog         backlog entered
--   created on a board             backlog skipped, earlier columns skipped, its column entered
--   moved to a column              columns jumped over (forward, or from outside the board) skipped, then entered
--   sent to the backlog            backlog entered
--   resolved (done/won't do/dup)   resolved entered (after any column it was moved to in the same change)
CREATE FUNCTION record_ticket_history() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    who      text := NULLIF(current_setting('tracklines.actor', true), '');
    to_pos   integer;
    from_pos integer;
    last_pos integer;
    col_name text;
    brd_name text;
BEGIN
    IF TG_OP = 'INSERT' THEN
        who := COALESCE(who, NEW.created_by);
    END IF;

    IF TG_OP = 'INSERT' OR OLD.column_id IS DISTINCT FROM NEW.column_id THEN
        IF NEW.column_id IS NULL THEN
            INSERT INTO ticket_history (ticket_id, project_id, stage, outcome, stage_order, actor)
            VALUES (NEW.id, NEW.project_id, 'backlog', 'entered', 0, who);
        ELSE
            SELECT c.position, c.name, b.name, (SELECT max(x.position) FROM columns x WHERE x.board_id = c.board_id)
                INTO to_pos, col_name, brd_name, last_pos
                FROM columns c JOIN boards b ON b.id = c.board_id WHERE c.id = NEW.column_id;
            -- moving along the same board starts from its old column; anything else enters the board fresh
            IF TG_OP = 'UPDATE' AND OLD.column_id IS NOT NULL AND OLD.board_id = NEW.board_id THEN
                SELECT position INTO from_pos FROM columns WHERE id = OLD.column_id;
            END IF;
            IF TG_OP = 'INSERT' THEN -- made straight on a board: it never sat in the backlog
                INSERT INTO ticket_history (ticket_id, project_id, stage, outcome, stage_order, actor)
                VALUES (NEW.id, NEW.project_id, 'backlog', 'skipped', 0, who);
            END IF;
            INSERT INTO ticket_history (ticket_id, project_id, stage, outcome, stage_order, board_id, board_name, column_id, column_name, actor)
                SELECT NEW.id, NEW.project_id, 'column', 'skipped', c.position + 1, NEW.board_id, brd_name, c.id, c.name, who
                FROM columns c WHERE c.board_id = NEW.board_id AND c.position < to_pos AND c.position > COALESCE(from_pos, -1)
                ORDER BY c.position;
            INSERT INTO ticket_history (ticket_id, project_id, stage, outcome, stage_order, board_id, board_name, column_id, column_name, done, actor)
            VALUES (NEW.id, NEW.project_id, 'column', 'entered', to_pos + 1, NEW.board_id, brd_name, NEW.column_id, col_name, to_pos = last_pos, who);
        END IF;
    END IF;

    IF NEW.resolution IS NOT NULL AND (TG_OP = 'INSERT' OR OLD.resolution IS DISTINCT FROM NEW.resolution) THEN
        INSERT INTO ticket_history (ticket_id, project_id, stage, outcome, stage_order, board_id, board_name, column_id, column_name, resolution, actor)
        SELECT NEW.id, NEW.project_id, 'resolved', 'entered', 1000000, NULL, NULL, NULL, NULL, NEW.resolution, who;
    END IF;
    RETURN NULL;
END $$;

CREATE TRIGGER record_ticket_history AFTER INSERT OR UPDATE OF column_id, board_id, resolution ON tickets_all
    FOR EACH ROW EXECUTE FUNCTION record_ticket_history();

-- audit_stage_id: a ticket_history row's event id. Deterministic, so the backfill (TL-667) sends rows
-- written before this migration under the same ids, and none is ever stored twice.
CREATE FUNCTION audit_stage_id(history_id bigint) RETURNS uuid LANGUAGE sql IMMUTABLE AS $$
    SELECT md5('ticket_history:' || history_id)::uuid
$$;

-- Each ticket stage is also queued for the auditor, in the same transaction (ticket_history keeps being
-- written until the cutover, TL-668, which moves this into record_ticket_history).
CREATE FUNCTION queue_ticket_stage() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO audit_outbox (id, org_id, at, actor, actor_kind, action, target_type, target_id, project_id, data)
    SELECT audit_stage_id(NEW.id), p.owner_clerk_id, NEW.at, NEW.actor, audit_actor_kind(NEW.actor), 'ticket.stage',
           'ticket', NEW.ticket_id::text, NEW.project_id,
           jsonb_strip_nulls(jsonb_build_object('stage', NEW.stage, 'outcome', NEW.outcome, 'stage_order', NEW.stage_order,
               'board_id', NEW.board_id, 'board_name', NEW.board_name, 'column_id', NEW.column_id,
               'column_name', NEW.column_name, 'resolution', NEW.resolution)) || jsonb_build_object('done', NEW.done)
    FROM projects p WHERE p.id = NEW.project_id;
    RETURN NULL;
END $$;
CREATE TRIGGER queue_ticket_stage AFTER INSERT ON ticket_history FOR EACH ROW EXECUTE FUNCTION queue_ticket_stage();
