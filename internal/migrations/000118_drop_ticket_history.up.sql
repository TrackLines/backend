-- TL-668: the audit cutover. Ticket history has been read from the auditor in production since TL-667;
-- the backend's own copy goes. Stages are queued for the auditor straight from the tickets trigger, in the
-- same transaction as the change, as before; they used to go through ticket_history first.

-- An import's fingerprints (internal/imports) counted a ticket's ticket_history rows. Tickets still as
-- they were imported get the fingerprint without that count, so their import can still be scrapped.
UPDATE import_items i SET fingerprint = md5(row_to_json(t)::text
        || coalesce((SELECT string_agg(lower(l.label), ',' ORDER BY lower(l.label)) FROM ticket_labels l WHERE l.ticket_id = t.id), '')
        || coalesce((SELECT string_agg(c.id::text || md5(c.body), ',' ORDER BY c.id) FROM ticket_comments c WHERE c.ticket_id = t.id), '')
        || (SELECT count(*) FROM ticket_attachments a WHERE a.ticket_id = t.id)
        || '/' || (SELECT count(*) FROM import_reviews r WHERE r.ticket_id = t.id))
    FROM tickets_all t
    WHERE i.kind = 'ticket' AND t.id = i.target_id AND i.fingerprint = md5(row_to_json(t)::text
        || coalesce((SELECT string_agg(lower(l.label), ',' ORDER BY lower(l.label)) FROM ticket_labels l WHERE l.ticket_id = t.id), '')
        || coalesce((SELECT string_agg(c.id::text || md5(c.body), ',' ORDER BY c.id) FROM ticket_comments c WHERE c.ticket_id = t.id), '')
        || (SELECT count(*) FROM ticket_attachments a WHERE a.ticket_id = t.id)
        || '/' || (SELECT count(*) FROM ticket_history h WHERE h.ticket_id = t.id)
        || '/' || (SELECT count(*) FROM import_reviews r WHERE r.ticket_id = t.id));

DROP TRIGGER record_ticket_history ON tickets_all;
DROP FUNCTION record_ticket_history();
DROP TABLE ticket_history; -- and its triggers: append-only, and the queueing below replaces queue_ticket_stage
DROP FUNCTION queue_ticket_stage();
DROP FUNCTION ticket_history_append_only();
DROP FUNCTION audit_stage_id(bigint);

-- audit_ticket_stage queues one stage of ticket tk for the auditor: the event ticket_history's trigger
-- used to queue, with a new id. A board and column only for a column stage, a resolution only for resolved.
CREATE FUNCTION audit_ticket_stage(tk tickets_all, _stage text, _outcome text, _order integer, _board text,
        _column_id uuid, _column text, _done boolean, _who text) RETURNS void LANGUAGE sql AS $$
    INSERT INTO audit_outbox (org_id, actor, actor_kind, action, target_type, target_id, project_id, data)
    SELECT p.owner_clerk_id, _who, audit_actor_kind(_who), 'ticket.stage', 'ticket', (tk).id::text, (tk).project_id,
           jsonb_strip_nulls(jsonb_build_object('stage', _stage, 'outcome', _outcome, 'stage_order', _order,
               'board_id', CASE WHEN _stage = 'column' THEN (tk).board_id END, 'board_name', _board, 'column_id', _column_id,
               'column_name', _column, 'resolution', CASE WHEN _stage = 'resolved' THEN (tk).resolution END))
               || jsonb_build_object('done', _done)
    FROM projects p WHERE p.id = (tk).project_id
$$;

-- The same stages as before (migration 000068), queued instead of stored:
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
    skipped  record;
BEGIN
    IF TG_OP = 'INSERT' THEN
        who := COALESCE(who, NEW.created_by);
    END IF;

    IF TG_OP = 'INSERT' OR OLD.column_id IS DISTINCT FROM NEW.column_id THEN
        IF NEW.column_id IS NULL THEN
            PERFORM audit_ticket_stage(NEW, 'backlog', 'entered', 0, NULL, NULL, NULL, false, who);
        ELSE
            SELECT c.position, c.name, b.name, (SELECT max(x.position) FROM columns x WHERE x.board_id = c.board_id)
                INTO to_pos, col_name, brd_name, last_pos
                FROM columns c JOIN boards b ON b.id = c.board_id WHERE c.id = NEW.column_id;
            -- moving along the same board starts from its old column; anything else enters the board fresh
            IF TG_OP = 'UPDATE' AND OLD.column_id IS NOT NULL AND OLD.board_id = NEW.board_id THEN
                SELECT position INTO from_pos FROM columns WHERE id = OLD.column_id;
            END IF;
            IF TG_OP = 'INSERT' THEN -- made straight on a board: it never sat in the backlog
                PERFORM audit_ticket_stage(NEW, 'backlog', 'skipped', 0, NULL, NULL, NULL, false, who);
            END IF;
            FOR skipped IN SELECT c.id, c.name, c.position FROM columns c
                WHERE c.board_id = NEW.board_id AND c.position < to_pos AND c.position > COALESCE(from_pos, -1) ORDER BY c.position
            LOOP
                PERFORM audit_ticket_stage(NEW, 'column', 'skipped', skipped.position + 1, brd_name, skipped.id, skipped.name, false, who);
            END LOOP;
            PERFORM audit_ticket_stage(NEW, 'column', 'entered', to_pos + 1, brd_name, NEW.column_id, col_name, to_pos = last_pos, who);
        END IF;
    END IF;

    IF NEW.resolution IS NOT NULL AND (TG_OP = 'INSERT' OR OLD.resolution IS DISTINCT FROM NEW.resolution) THEN
        PERFORM audit_ticket_stage(NEW, 'resolved', 'entered', 1000000, NULL, NULL, NULL, false, who);
    END IF;
    RETURN NULL;
END $$;

CREATE TRIGGER record_ticket_history AFTER INSERT OR UPDATE OF column_id, board_id, resolution ON tickets_all
    FOR EACH ROW EXECUTE FUNCTION record_ticket_history();
