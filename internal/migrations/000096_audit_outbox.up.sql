-- The audit outbox (TL-666): events for the auditor service, written in the same transaction as the change
-- that caused them, then relayed (internal/audit). A change never waits on the auditor, and nothing is lost
-- if it's down: rows wait here until it accepts them.
CREATE TABLE audit_outbox (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(), -- the event's id in the auditor: a resend is a no-op
    queued          BIGINT GENERATED ALWAYS AS IDENTITY,        -- the order to send in
    org_id          TEXT NOT NULL,
    at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    actor           TEXT,                                       -- user id, agent or service key name; null: the system
    actor_kind      TEXT NOT NULL CHECK (actor_kind IN ('person', 'agent', 'service', 'system')),
    action          TEXT NOT NULL,
    target_type     TEXT,
    target_id       TEXT,
    project_id      UUID,
    data            JSONB NOT NULL DEFAULT '{}',
    attempts        INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX audit_outbox_due_idx ON audit_outbox (next_attempt_at, queued);
CREATE INDEX audit_outbox_target_idx ON audit_outbox (org_id, target_type, target_id); -- a ticket's unsent stages (TL-667)

-- audit_actor_kind: who's acting, as the auditor names it. The app says (tracklines.actor_kind, set with
-- tracklines.actor by boards.SetActor); where it didn't (a ticket created through a path that doesn't set
-- it), a Clerk user id is a person and any other name an agent. No actor: the system.
CREATE FUNCTION audit_actor_kind(who text) RETURNS text LANGUAGE sql STABLE AS $$
    SELECT CASE
        WHEN who IS NULL THEN 'system'
        ELSE COALESCE(NULLIF(current_setting('tracklines.actor_kind', true), ''),
                      CASE WHEN who LIKE 'user\_%' THEN 'person' ELSE 'agent' END)
    END
$$;

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

-- A ticket deleted for good (its 30 days in the bin are up, or an import scrapped): the auditor deletes its
-- stages (TL-665). Inside an organization's purge its project has already gone, so nothing is queued:
-- org.purged covers everything then.
CREATE FUNCTION queue_ticket_purged() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO audit_outbox (org_id, actor_kind, action, target_type, target_id, project_id)
    SELECT p.owner_clerk_id, 'system', 'ticket.purged', 'ticket', OLD.id::text, OLD.project_id
    FROM projects p WHERE p.id = OLD.project_id;
    RETURN NULL;
END $$;
CREATE TRIGGER queue_ticket_purged AFTER DELETE ON tickets_all FOR EACH ROW EXECUTE FUNCTION queue_ticket_purged();

-- The org's plan as the auditor keeps it for retention (TL-676): the same rule as everywhere else, an
-- active or trialing subscription is its plan (enterprise when none is named), anything else is free.
CREATE FUNCTION audit_plan(status text, plan text) RETURNS text LANGUAGE sql IMMUTABLE AS $$
    SELECT CASE WHEN status IN ('active', 'trialing') THEN COALESCE(plan, 'enterprise') ELSE 'free' END
$$;

CREATE FUNCTION queue_plan_changed() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    was text := CASE WHEN TG_OP = 'UPDATE' THEN audit_plan(OLD.stripe_status, OLD.plan) ELSE 'free' END;
    now_plan text := audit_plan(NEW.stripe_status, NEW.plan);
BEGIN
    IF now_plan IS DISTINCT FROM was THEN
        INSERT INTO audit_outbox (org_id, actor_kind, action, target_type, target_id, data)
        VALUES (NEW.org_id, 'system', 'org.plan_changed', 'org', NEW.org_id, jsonb_build_object('plan', now_plan, 'from', was));
    END IF;
    RETURN NULL;
END $$;
CREATE TRIGGER queue_plan_changed AFTER INSERT OR UPDATE OF stripe_status, plan ON org_billing
    FOR EACH ROW EXECUTE FUNCTION queue_plan_changed();
