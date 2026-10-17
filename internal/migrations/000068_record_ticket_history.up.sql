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
