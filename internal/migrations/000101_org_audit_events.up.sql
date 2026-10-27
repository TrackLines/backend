-- Who did what across the organization, for its audit log (TL-669). Queued with each change, like the
-- ticket activity (000098), so every path is covered: API, MCP, the Clerk webhook, imports. Pro and
-- Enterprise only: a Free organization's org log isn't recorded (data minimisation); one that upgrades
-- starts its log then. Ids, and names of org-level things as they were; never secrets, keys, webhook
-- paths or ticket text.

-- audit_org queues action on a target for org, when it's on a paid plan. Who did it is the connection's
-- tracklines.actor (boards.ActorHook names every request's caller), else fallback, else the system.
CREATE FUNCTION audit_org(org text, act text, ttype text, tid text, project uuid, d jsonb, fallback text DEFAULT NULL)
RETURNS void LANGUAGE sql AS $$
    INSERT INTO audit_outbox (org_id, actor, actor_kind, action, target_type, target_id, project_id, data)
    SELECT org, w.who, audit_actor_kind(w.who), act, ttype, tid, project, d
    FROM (SELECT COALESCE(NULLIF(current_setting('tracklines.actor', true), ''), NULLIF(fallback, '')) AS who) w
    WHERE org IS NOT NULL
      AND EXISTS (SELECT 1 FROM org_billing b WHERE b.org_id = org AND audit_plan(b.stripe_status, b.plan) <> 'free')
$$;

-- Projects: created, renamed, deleted (with their name at the time).
CREATE FUNCTION queue_project() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        PERFORM audit_org(NEW.owner_clerk_id, 'project.created', 'project', NEW.id::text, NEW.id, jsonb_build_object('name', NEW.name));
    ELSIF TG_OP = 'DELETE' THEN
        PERFORM audit_org(OLD.owner_clerk_id, 'project.deleted', 'project', OLD.id::text, NULL, jsonb_build_object('name', OLD.name));
    ELSIF OLD.name IS DISTINCT FROM NEW.name THEN
        PERFORM audit_org(NEW.owner_clerk_id, 'project.renamed', 'project', NEW.id::text, NEW.id, audit_fromto(OLD.name, NEW.name));
    END IF;
    RETURN NULL;
END $$;
CREATE TRIGGER queue_project AFTER INSERT OR UPDATE OF name OR DELETE ON projects FOR EACH ROW EXECUTE FUNCTION queue_project();

-- Boards: created, renamed, archived, restored, deleted.
CREATE FUNCTION queue_board() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        PERFORM audit_org(NEW.owner_clerk_id, 'board.created', 'board', NEW.id::text, NEW.project_id, jsonb_build_object('name', NEW.name));
        RETURN NULL;
    ELSIF TG_OP = 'DELETE' THEN
        PERFORM audit_org(OLD.owner_clerk_id, 'board.deleted', 'board', OLD.id::text, OLD.project_id, jsonb_build_object('name', OLD.name));
        RETURN NULL;
    END IF;
    IF OLD.name IS DISTINCT FROM NEW.name THEN
        PERFORM audit_org(NEW.owner_clerk_id, 'board.renamed', 'board', NEW.id::text, NEW.project_id, audit_fromto(OLD.name, NEW.name));
    END IF;
    IF OLD.archived_at IS NULL AND NEW.archived_at IS NOT NULL THEN
        PERFORM audit_org(NEW.owner_clerk_id, 'board.archived', 'board', NEW.id::text, NEW.project_id, jsonb_build_object('name', NEW.name));
    ELSIF OLD.archived_at IS NOT NULL AND NEW.archived_at IS NULL THEN
        PERFORM audit_org(NEW.owner_clerk_id, 'board.restored', 'board', NEW.id::text, NEW.project_id, jsonb_build_object('name', NEW.name));
    END IF;
    RETURN NULL;
END $$;
CREATE TRIGGER queue_board AFTER INSERT OR UPDATE OF name, archived_at OR DELETE ON boards FOR EACH ROW EXECUTE FUNCTION queue_board();

-- Teams: created and deleted; who's in them (and who leads) changing.
CREATE FUNCTION queue_team() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        PERFORM audit_org(NEW.org_id, 'team.created', 'team', NEW.id::text, NULL, jsonb_build_object('name', NEW.name));
    ELSE
        PERFORM audit_org(OLD.org_id, 'team.deleted', 'team', OLD.id::text, NULL, jsonb_build_object('name', OLD.name));
    END IF;
    RETURN NULL;
END $$;
CREATE TRIGGER queue_team AFTER INSERT OR DELETE ON teams FOR EACH ROW EXECUTE FUNCTION queue_team();

-- Nothing once the team itself is gone: its deletion says it.
CREATE FUNCTION queue_team_member() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    r team_members := CASE WHEN TG_OP = 'DELETE' THEN OLD ELSE NEW END;
    change text := CASE
        WHEN TG_OP = 'INSERT' THEN 'added'
        WHEN TG_OP = 'DELETE' THEN 'removed'
        WHEN NEW.leader THEN 'made_leader'
        ELSE 'no_longer_leader' END;
BEGIN
    IF TG_OP = 'UPDATE' AND OLD.leader = NEW.leader THEN
        RETURN NULL;
    END IF;
    PERFORM audit_org(t.org_id, 'team.members_changed', 'team', t.id::text, NULL,
        jsonb_build_object('name', t.name, 'user', r.user_clerk_id, 'change', change, 'leader', r.leader))
    FROM teams t WHERE t.id = r.team_id;
    RETURN NULL;
END $$;
CREATE TRIGGER queue_team_member AFTER INSERT OR UPDATE OF leader OR DELETE ON team_members FOR EACH ROW EXECUTE FUNCTION queue_team_member();

-- Sprints: started and closed.
CREATE FUNCTION queue_sprint() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        PERFORM audit_org(b.owner_clerk_id, 'sprint.started', 'sprint', NEW.id::text, b.project_id,
            jsonb_build_object('board', b.id, 'board_name', b.name, 'number', NEW.number, 'length_days', NEW.length_days))
        FROM boards b WHERE b.id = NEW.board_id;
    ELSIF OLD.closed_at IS NULL AND NEW.closed_at IS NOT NULL THEN
        PERFORM audit_org(b.owner_clerk_id, 'sprint.closed', 'sprint', NEW.id::text, b.project_id,
            jsonb_build_object('board', b.id, 'board_name', b.name, 'number', NEW.number))
        FROM boards b WHERE b.id = NEW.board_id;
    END IF;
    RETURN NULL;
END $$;
CREATE TRIGGER queue_sprint AFTER INSERT OR UPDATE OF closed_at ON sprints FOR EACH ROW EXECUTE FUNCTION queue_sprint();

-- API keys: made and revoked; what kind, its name, read-only, expiry. Never the key.
CREATE FUNCTION queue_api_key() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    d jsonb := jsonb_build_object('kind', NEW.kind, 'name', NEW.name, 'read_only', NEW.read_only, 'expires_at', NEW.expires_at, 'owner', NEW.owner_clerk_id);
BEGIN
    IF TG_OP = 'INSERT' THEN
        PERFORM audit_org(NEW.org_id, 'api_key.created', 'api_key', NEW.id::text, NULL, d, NEW.owner_clerk_id);
    ELSIF OLD.revoked_at IS NULL AND NEW.revoked_at IS NOT NULL THEN
        PERFORM audit_org(NEW.org_id, 'api_key.revoked', 'api_key', NEW.id::text, NULL, d);
    END IF;
    RETURN NULL;
END $$;
CREATE TRIGGER queue_api_key AFTER INSERT OR UPDATE OF revoked_at ON api_keys FOR EACH ROW EXECUTE FUNCTION queue_api_key();

-- Invitations: sent and withdrawn (joining is Clerk's membership event). What to, never the address.
CREATE FUNCTION queue_invitation() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    d jsonb := jsonb_build_object('scope', NEW.scope, 'target', NEW.target_id);
BEGIN
    IF TG_OP = 'INSERT' THEN
        PERFORM audit_org(NEW.org_id, 'member.invited', 'invitation', NEW.id::text, NULL, d, NEW.invited_by);
    ELSIF OLD.status = 'pending' AND NEW.status = 'revoked' THEN
        PERFORM audit_org(NEW.org_id, 'member.invitation_revoked', 'invitation', NEW.id::text, NULL, d);
    END IF;
    RETURN NULL;
END $$;
CREATE TRIGGER queue_invitation AFTER INSERT OR UPDATE OF status ON invitations FOR EACH ROW EXECUTE FUNCTION queue_invitation();

-- Org admins: granted and revoked.
CREATE FUNCTION queue_org_admin() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        PERFORM audit_org(NEW.org_id, 'admin.granted', 'user', NEW.user_clerk_id, NULL, '{}', NEW.granted_by);
    ELSE
        PERFORM audit_org(OLD.org_id, 'admin.revoked', 'user', OLD.user_clerk_id, NULL, '{}');
    END IF;
    RETURN NULL;
END $$;
CREATE TRIGGER queue_org_admin AFTER INSERT OR DELETE ON org_admins FOR EACH ROW EXECUTE FUNCTION queue_org_admin();

-- audit_host: a URL's host, without its path or query (which may carry a token).
CREATE FUNCTION audit_host(u text) RETURNS text LANGUAGE sql IMMUTABLE AS $$
    SELECT lower(substring(u FROM '^[A-Za-z][A-Za-z0-9+.-]*://(?:[^@/?#]*@)?([^/:?#]+)'))
$$;

-- Outgoing webhooks: made, changed (where, which events, on or off), deleted. Never the secret.
CREATE FUNCTION queue_webhook() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    r webhook_endpoints := CASE WHEN TG_OP = 'DELETE' THEN OLD ELSE NEW END;
    d jsonb := jsonb_build_object('host', audit_host(r.url), 'events', to_jsonb(r.events), 'enabled', r.enabled);
BEGIN
    IF TG_OP = 'INSERT' THEN
        PERFORM audit_org(NEW.org_id, 'webhook.created', 'webhook', NEW.id::text, NULL, d, NEW.created_by);
    ELSIF TG_OP = 'DELETE' THEN
        PERFORM audit_org(OLD.org_id, 'webhook.deleted', 'webhook', OLD.id::text, NULL, d);
    ELSIF (OLD.url, OLD.events, OLD.enabled) IS DISTINCT FROM (NEW.url, NEW.events, NEW.enabled) THEN
        PERFORM audit_org(NEW.org_id, 'webhook.updated', 'webhook', NEW.id::text, NULL, d);
    END IF;
    RETURN NULL;
END $$;
CREATE TRIGGER queue_webhook AFTER INSERT OR UPDATE OF url, events, enabled OR DELETE ON webhook_endpoints FOR EACH ROW EXECUTE FUNCTION queue_webhook();

-- Integrations connected and disconnected: one trigger per table, the provider named in TG_ARGV[0]
-- (or the row's own, for org_integrations). Reconnecting an existing one isn't a new connection.
CREATE FUNCTION queue_integration() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    r jsonb := to_jsonb(CASE WHEN TG_OP = 'DELETE' THEN OLD ELSE NEW END);
    provider text := COALESCE(r->>'provider', TG_ARGV[0]);
    d jsonb := jsonb_strip_nulls(jsonb_build_object('provider', provider, 'account', NULLIF(r->>'account', ''),
        'site', r->>'site', 'workspace', r->>'workspace', 'domain', r->>'domain', 'project', r->>'project_id'));
    by_whom text := COALESCE(r->>'connected_by', r->>'created_by', r->>'updated_by');
BEGIN
    IF TG_OP = 'INSERT' THEN
        PERFORM audit_org(r->>'org_id', 'integration.connected', 'integration', provider, (r->>'project_id')::uuid, d, by_whom);
    ELSIF TG_OP = 'DELETE' THEN
        PERFORM audit_org(r->>'org_id', 'integration.disconnected', 'integration', provider, (r->>'project_id')::uuid, d);
    ELSIF to_jsonb(OLD)->>'domain' IS DISTINCT FROM r->>'domain' THEN
        PERFORM audit_org(r->>'org_id', 'calendar.domain_changed', 'integration', provider, NULL, audit_fromto(to_jsonb(OLD)->>'domain', r->>'domain'));
    END IF;
    RETURN NULL;
END $$;
CREATE TRIGGER queue_integration AFTER INSERT OR DELETE ON org_integrations FOR EACH ROW EXECUTE FUNCTION queue_integration();
CREATE TRIGGER queue_integration AFTER INSERT OR UPDATE OF domain OR DELETE ON calendar_connections FOR EACH ROW EXECUTE FUNCTION queue_integration('google-calendar');
CREATE TRIGGER queue_integration AFTER INSERT OR DELETE ON jira_connections FOR EACH ROW EXECUTE FUNCTION queue_integration('jira');
CREATE TRIGGER queue_integration AFTER INSERT OR DELETE ON linear_connections FOR EACH ROW EXECUTE FUNCTION queue_integration('linear');
CREATE TRIGGER queue_integration AFTER INSERT OR DELETE ON chat_webhooks FOR EACH ROW EXECUTE FUNCTION queue_integration('google-chat');

-- Imports: once one finishes (done, failed or cancelled), where from and how many.
CREATE FUNCTION queue_import() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.state IN ('done', 'failed', 'cancelled') AND OLD.state IS DISTINCT FROM NEW.state THEN
        PERFORM audit_org(NEW.org_id, 'import.run', 'import', NEW.id::text, NULL,
            jsonb_build_object('source', NEW.source, 'state', NEW.state, 'created', NEW.progress, 'total', NEW.total), NEW.created_by);
    END IF;
    RETURN NULL;
END $$;
CREATE TRIGGER queue_import AFTER UPDATE OF state ON imports FOR EACH ROW EXECUTE FUNCTION queue_import();

-- The organization deleted (in Clerk): kept 30 days, then purged.
CREATE FUNCTION queue_org_deleted() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM audit_org(NEW.org_id, 'org.deleted', 'org', NEW.org_id, NULL, '{}');
    RETURN NULL;
END $$;
CREATE TRIGGER queue_org_deleted AFTER INSERT ON deleted_orgs FOR EACH ROW EXECUTE FUNCTION queue_org_deleted();
