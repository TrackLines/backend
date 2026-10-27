-- The org audit log's gaps (TL-700, part of TL-624): settings changes with before and after, the request
-- form, embedding, repositories, integration settings, roadmap visibility, scrapped imports and expired API
-- keys. Paid plans only, through audit_org (000101), like the rest of the org log. Never a secret or token.

-- audit_changes: {field: {from, to}} for the named fields that differ between two rows (as jsonb).
CREATE FUNCTION audit_changes(old_row jsonb, new_row jsonb, fields text[]) RETURNS jsonb LANGUAGE sql IMMUTABLE AS $$
    SELECT COALESCE(jsonb_object_agg(f, jsonb_build_object('from', old_row->f, 'to', new_row->f)), '{}')
    FROM unnest(fields) f WHERE old_row->f IS DISTINCT FROM new_row->f
$$;

-- Board settings, one event for whatever changed in one save.
CREATE FUNCTION queue_board_settings() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    changed jsonb := audit_changes(to_jsonb(OLD), to_jsonb(NEW), ARRAY['estimate_scale', 'style', 'agent_review',
        'dev_complete_column_id', 'review_column_id', 'when_sprint_ends', 'team_id']);
BEGIN
    IF changed <> '{}' THEN
        PERFORM audit_org(NEW.owner_clerk_id, 'board.settings_changed', 'board', NEW.id::text, NEW.project_id,
            jsonb_build_object('name', NEW.name, 'fields', changed));
    END IF;
    RETURN NULL;
END $$;
CREATE TRIGGER queue_board_settings AFTER UPDATE OF estimate_scale, style, agent_review, dev_complete_column_id, review_column_id,
    when_sprint_ends, team_id ON boards FOR EACH ROW EXECUTE FUNCTION queue_board_settings();

-- The project's request form: opened, closed or reset (a new link). Never the token.
CREATE FUNCTION queue_request_form() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    change text := CASE WHEN OLD.intake_token IS NULL THEN 'opened' WHEN NEW.intake_token IS NULL THEN 'closed' ELSE 'reset' END;
BEGIN
    IF OLD.intake_token IS DISTINCT FROM NEW.intake_token THEN
        PERFORM audit_org(NEW.owner_clerk_id, 'project.request_form', 'project', NEW.id::text, NEW.id,
            jsonb_build_object('name', NEW.name, 'change', change));
    END IF;
    RETURN NULL;
END $$;
CREATE TRIGGER queue_request_form AFTER UPDATE OF intake_token ON projects FOR EACH ROW EXECUTE FUNCTION queue_request_form();

-- Embedding: sites allowed and removed, keys made and deleted (by label; the key is public but says nothing).
CREATE FUNCTION queue_embed() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    r jsonb := to_jsonb(CASE WHEN TG_OP = 'DELETE' THEN OLD ELSE NEW END);
    act text := CASE TG_TABLE_NAME
        WHEN 'embed_origins' THEN CASE WHEN TG_OP = 'INSERT' THEN 'embed.site_added' ELSE 'embed.site_removed' END
        ELSE CASE WHEN TG_OP = 'INSERT' THEN 'embed.key_created' ELSE 'embed.key_deleted' END END;
BEGIN
    PERFORM audit_org(r->>'org', act, 'project', r->>'project_id', (r->>'project_id')::uuid,
        jsonb_strip_nulls(jsonb_build_object('origin', r->>'origin', 'label', r->>'label')), NULLIF(r->>'created_by', ''));
    RETURN NULL;
END $$;
CREATE TRIGGER queue_embed AFTER INSERT OR DELETE ON embed_origins FOR EACH ROW EXECUTE FUNCTION queue_embed();
CREATE TRIGGER queue_embed AFTER INSERT OR DELETE ON embed_keys FOR EACH ROW EXECUTE FUNCTION queue_embed();

-- Repositories linked to a project and unlinked.
CREATE FUNCTION queue_repo() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    r project_repos := CASE WHEN TG_OP = 'DELETE' THEN OLD ELSE NEW END;
BEGIN
    PERFORM audit_org(r.org_id, CASE WHEN TG_OP = 'INSERT' THEN 'repo.linked' ELSE 'repo.unlinked' END, 'project', r.project_id::text,
        r.project_id, jsonb_build_object('provider', r.provider, 'repo', r.full_name));
    RETURN NULL;
END $$;
CREATE TRIGGER queue_repo AFTER INSERT OR DELETE ON project_repos FOR EACH ROW EXECUTE FUNCTION queue_repo();

-- An integration's settings changing while it stays connected: what changed, never a secret or a webhook URL.
CREATE FUNCTION queue_integration_settings() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    changed jsonb := audit_changes(to_jsonb(OLD), to_jsonb(NEW), TG_ARGV[1:]);
BEGIN
    IF to_jsonb(OLD)->>'base_url' IS DISTINCT FROM to_jsonb(NEW)->>'base_url' THEN -- only the host
        changed := changed || jsonb_build_object('base_url', audit_fromto(audit_host(to_jsonb(OLD)->>'base_url'), audit_host(to_jsonb(NEW)->>'base_url')));
    END IF;
    IF changed <> '{}' THEN
        PERFORM audit_org(NEW.org_id, 'integration.updated', 'integration', COALESCE(to_jsonb(NEW)->>'provider', TG_ARGV[0]),
            (to_jsonb(NEW)->>'project_id')::uuid, jsonb_build_object('provider', COALESCE(to_jsonb(NEW)->>'provider', TG_ARGV[0]), 'fields', changed));
    END IF;
    RETURN NULL;
END $$;
CREATE TRIGGER queue_integration_settings AFTER UPDATE OF kinds ON chat_webhooks FOR EACH ROW EXECUTE FUNCTION queue_integration_settings('google-chat', 'kinds');
CREATE TRIGGER queue_integration_settings AFTER UPDATE OF base_url, account ON org_integrations FOR EACH ROW EXECUTE FUNCTION queue_integration_settings('', 'account');
CREATE TRIGGER queue_integration_settings AFTER UPDATE OF holidays_calendar ON calendar_connections FOR EACH ROW EXECUTE FUNCTION queue_integration_settings('google-calendar', 'holidays_calendar');

-- Who can see a roadmap.
CREATE FUNCTION queue_roadmap_visibility() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.visibility IS DISTINCT FROM NEW.visibility THEN
        PERFORM audit_org(NEW.owner_clerk_id, 'roadmap.visibility_changed', 'roadmap', NEW.id::text, NEW.project_id,
            jsonb_build_object('title', NEW.title, 'from', OLD.visibility, 'to', NEW.visibility));
    END IF;
    RETURN NULL;
END $$;
CREATE TRIGGER queue_roadmap_visibility AFTER UPDATE OF visibility ON roadmaps FOR EACH ROW EXECUTE FUNCTION queue_roadmap_visibility();

-- Imports: a scrapped one is recorded too.
CREATE OR REPLACE FUNCTION queue_import() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.state IN ('done', 'failed', 'cancelled', 'scrapped') AND OLD.state IS DISTINCT FROM NEW.state THEN
        PERFORM audit_org(NEW.org_id, 'import.run', 'import', NEW.id::text, NULL,
            jsonb_build_object('source', NEW.source, 'state', NEW.state, 'created', NEW.progress, 'total', NEW.total), NEW.created_by);
    END IF;
    RETURN NULL;
END $$;

-- An API key's expiry is recorded once, by an hourly sweep (audit.ExpireKeys): this marks it done.
ALTER TABLE api_keys ADD COLUMN expiry_recorded boolean NOT NULL DEFAULT false;
-- keys already past their expiry are history: don't record them all at once on the first sweep
UPDATE api_keys SET expiry_recorded = true WHERE expires_at <= now();
