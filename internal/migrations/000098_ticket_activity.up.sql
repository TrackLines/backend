-- Every change to a ticket, for the auditor (TL-677; the owner: "make audit full"). Queued with the change,
-- like the stages (000096), so every path is covered: API, MCP, bulk edits, assign-on-move, imports, sprints.
-- Text (title, description, comment bodies) is recorded as changed, never what it says. Who did it is the
-- connection's tracklines.actor (boards.ActorHook names every request's caller), falling back to the row's
-- own author where it has one.

-- audit_ticket queues action on ticket t, with data d; fallback is who did it when no caller is named.
-- Nothing for a ticket that's gone (a purge cascading to its labels, comments and files).
CREATE FUNCTION audit_ticket(t uuid, act text, d jsonb, fallback text DEFAULT NULL) RETURNS void LANGUAGE sql AS $$
    INSERT INTO audit_outbox (org_id, actor, actor_kind, action, target_type, target_id, project_id, data)
    SELECT p.owner_clerk_id, w.who, audit_actor_kind(w.who), act, 'ticket', t::text, tk.project_id, d
    FROM tickets_all tk JOIN projects p ON p.id = tk.project_id
    CROSS JOIN LATERAL (SELECT COALESCE(NULLIF(current_setting('tracklines.actor', true), ''), fallback) AS who) w
    WHERE tk.id = t
$$;

-- fromto: {"from": a, "to": b}
CREATE FUNCTION audit_fromto(a anyelement, b anyelement) RETURNS jsonb LANGUAGE sql IMMUTABLE AS $$
    SELECT jsonb_build_object('from', a, 'to', b)
$$;

-- The ticket itself: one event per kind of change. Moves and resolving are stages already (000068).
CREATE FUNCTION queue_ticket_changes() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    fields jsonb := '{}';
BEGIN
    IF OLD.deleted_at IS NULL AND NEW.deleted_at IS NOT NULL THEN
        PERFORM audit_ticket(NEW.id, 'ticket.deleted', '{}');
    ELSIF OLD.deleted_at IS NOT NULL AND NEW.deleted_at IS NULL THEN
        PERFORM audit_ticket(NEW.id, 'ticket.restored', '{}');
    END IF;
    IF OLD.assigned_to IS DISTINCT FROM NEW.assigned_to THEN
        PERFORM audit_ticket(NEW.id, 'ticket.assigned', audit_fromto(OLD.assigned_to, NEW.assigned_to));
    END IF;
    IF OLD.resolution IS NOT NULL AND NEW.resolution IS NULL THEN
        PERFORM audit_ticket(NEW.id, 'ticket.reopened', jsonb_build_object('was', OLD.resolution));
    END IF;
    IF NEW.duplicate_of IS NOT NULL AND OLD.duplicate_of IS DISTINCT FROM NEW.duplicate_of THEN
        PERFORM audit_ticket(NEW.id, 'ticket.duplicate_of', jsonb_build_object('of', NEW.duplicate_of));
    END IF;
    IF OLD.parent_id IS DISTINCT FROM NEW.parent_id THEN
        PERFORM audit_ticket(NEW.id, 'ticket.parent_changed', audit_fromto(OLD.parent_id, NEW.parent_id));
    END IF;
    IF OLD.sprint_id IS DISTINCT FROM NEW.sprint_id THEN
        PERFORM audit_ticket(NEW.id, 'ticket.sprint_changed', audit_fromto(OLD.sprint_id, NEW.sprint_id));
    END IF;
    IF OLD.planned_sprint_id IS DISTINCT FROM NEW.planned_sprint_id THEN
        PERFORM audit_ticket(NEW.id, 'ticket.planned_changed', audit_fromto(OLD.planned_sprint_id, NEW.planned_sprint_id));
    END IF;
    IF OLD.review_requested_at IS NULL AND NEW.review_requested_at IS NOT NULL THEN
        PERFORM audit_ticket(NEW.id, 'ticket.review_requested', '{}');
    ELSIF OLD.review_requested_at IS NOT NULL AND NEW.review_requested_at IS NULL THEN
        PERFORM audit_ticket(NEW.id, 'ticket.review_ended', '{}');
    END IF;
    -- fields: values for the short ones, only "changed" for text
    IF OLD.type IS DISTINCT FROM NEW.type THEN fields := fields || jsonb_build_object('type', audit_fromto(OLD.type::text, NEW.type::text)); END IF;
    IF OLD.priority IS DISTINCT FROM NEW.priority THEN fields := fields || jsonb_build_object('priority', audit_fromto(OLD.priority::text, NEW.priority::text)); END IF;
    IF OLD.estimate IS DISTINCT FROM NEW.estimate THEN fields := fields || jsonb_build_object('estimate', audit_fromto(OLD.estimate, NEW.estimate)); END IF;
    IF OLD.due_date IS DISTINCT FROM NEW.due_date THEN fields := fields || jsonb_build_object('due_date', audit_fromto(OLD.due_date, NEW.due_date)); END IF;
    IF OLD.timebox_days IS DISTINCT FROM NEW.timebox_days THEN fields := fields || jsonb_build_object('timebox_days', audit_fromto(OLD.timebox_days, NEW.timebox_days)); END IF;
    IF OLD.timebox_hours IS DISTINCT FROM NEW.timebox_hours THEN fields := fields || jsonb_build_object('timebox_hours', audit_fromto(OLD.timebox_hours, NEW.timebox_hours)); END IF;
    IF OLD.title IS DISTINCT FROM NEW.title THEN fields := fields || '{"title": true}'; END IF;
    IF OLD.description IS DISTINCT FROM NEW.description THEN fields := fields || '{"description": true}'; END IF;
    IF fields <> '{}' THEN
        PERFORM audit_ticket(NEW.id, 'ticket.updated', jsonb_build_object('fields', fields));
    END IF;
    RETURN NULL;
END $$;
CREATE TRIGGER queue_ticket_changes AFTER UPDATE ON tickets_all FOR EACH ROW EXECUTE FUNCTION queue_ticket_changes();

-- Labels and blockers: one event per ticket per statement, listing what was added or removed (the app
-- writes only the difference).
CREATE FUNCTION queue_labels_added() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM audit_ticket(ticket_id, 'ticket.labels_changed', jsonb_build_object('added', jsonb_agg(label ORDER BY label)))
    FROM changed GROUP BY ticket_id;
    RETURN NULL;
END $$;
CREATE FUNCTION queue_labels_removed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM audit_ticket(ticket_id, 'ticket.labels_changed', jsonb_build_object('removed', jsonb_agg(label ORDER BY label)))
    FROM changed GROUP BY ticket_id;
    RETURN NULL;
END $$;
CREATE TRIGGER queue_labels_added AFTER INSERT ON ticket_labels REFERENCING NEW TABLE AS changed
    FOR EACH STATEMENT EXECUTE FUNCTION queue_labels_added();
CREATE TRIGGER queue_labels_removed AFTER DELETE ON ticket_labels REFERENCING OLD TABLE AS changed
    FOR EACH STATEMENT EXECUTE FUNCTION queue_labels_removed();

CREATE FUNCTION queue_blockers_added() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM audit_ticket(ticket_id, 'ticket.blocked_by_changed', jsonb_build_object('added', jsonb_agg(blocked_by_id ORDER BY blocked_by_id)))
    FROM changed GROUP BY ticket_id;
    RETURN NULL;
END $$;
CREATE FUNCTION queue_blockers_removed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM audit_ticket(ticket_id, 'ticket.blocked_by_changed', jsonb_build_object('removed', jsonb_agg(blocked_by_id ORDER BY blocked_by_id)))
    FROM changed GROUP BY ticket_id;
    RETURN NULL;
END $$;
CREATE TRIGGER queue_blockers_added AFTER INSERT ON ticket_dependencies REFERENCING NEW TABLE AS changed
    FOR EACH STATEMENT EXECUTE FUNCTION queue_blockers_added();
CREATE TRIGGER queue_blockers_removed AFTER DELETE ON ticket_dependencies REFERENCING OLD TABLE AS changed
    FOR EACH STATEMENT EXECUTE FUNCTION queue_blockers_removed();

-- Comments: that one was added, edited or deleted, never its body.
CREATE FUNCTION queue_comment() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        PERFORM audit_ticket(NEW.ticket_id, 'comment.added', jsonb_strip_nulls(jsonb_build_object('comment_id', NEW.id, 'reply_to', NEW.parent_comment_id)), NEW.author);
    ELSIF TG_OP = 'UPDATE' AND OLD.body IS DISTINCT FROM NEW.body THEN
        PERFORM audit_ticket(NEW.ticket_id, 'comment.edited', jsonb_build_object('comment_id', NEW.id));
    ELSIF TG_OP = 'DELETE' THEN
        PERFORM audit_ticket(OLD.ticket_id, 'comment.deleted', jsonb_build_object('comment_id', OLD.id));
    END IF;
    RETURN NULL;
END $$;
CREATE TRIGGER queue_comment AFTER INSERT OR UPDATE OF body OR DELETE ON ticket_comments FOR EACH ROW EXECUTE FUNCTION queue_comment();

-- Files: added, removed, kept for another year. The name and size are what the ticket already shows.
CREATE FUNCTION queue_attachment() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        PERFORM audit_ticket(NEW.ticket_id, 'attachment.added', jsonb_build_object('attachment_id', NEW.id, 'name', NEW.name, 'size', NEW.size_bytes), NEW.created_by);
    ELSIF TG_OP = 'UPDATE' AND NEW.kept_at IS DISTINCT FROM OLD.kept_at AND NEW.kept_at IS NOT NULL THEN
        PERFORM audit_ticket(NEW.ticket_id, 'attachment.kept', jsonb_build_object('attachment_id', NEW.id, 'name', NEW.name));
    ELSIF TG_OP = 'DELETE' THEN
        PERFORM audit_ticket(OLD.ticket_id, 'attachment.removed', jsonb_build_object('attachment_id', OLD.id, 'name', OLD.name));
    END IF;
    RETURN NULL;
END $$;
CREATE TRIGGER queue_attachment AFTER INSERT OR UPDATE OF kept_at OR DELETE ON ticket_attachments FOR EACH ROW EXECUTE FUNCTION queue_attachment();

-- Pull requests: linked, unlinked, and their state (opened, merged, closed) as GitHub or Bitbucket reports it.
CREATE FUNCTION queue_pr() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        PERFORM audit_ticket(NEW.ticket_id, 'pr.linked', jsonb_build_object('provider', NEW.provider, 'repo', NEW.repo, 'number', NEW.number, 'state', NEW.state));
    ELSIF TG_OP = 'UPDATE' AND OLD.state IS DISTINCT FROM NEW.state THEN
        PERFORM audit_ticket(NEW.ticket_id, 'pr.state_changed', jsonb_build_object('provider', NEW.provider, 'repo', NEW.repo, 'number', NEW.number) || audit_fromto(OLD.state, NEW.state));
    ELSIF TG_OP = 'DELETE' THEN
        PERFORM audit_ticket(OLD.ticket_id, 'pr.unlinked', jsonb_build_object('provider', OLD.provider, 'repo', OLD.repo, 'number', OLD.number));
    END IF;
    RETURN NULL;
END $$;
CREATE TRIGGER queue_pr AFTER INSERT OR UPDATE OF state OR DELETE ON ticket_prs FOR EACH ROW EXECUTE FUNCTION queue_pr();
