ALTER TABLE api_keys DROP COLUMN expiry_recorded;
CREATE OR REPLACE FUNCTION queue_import() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.state IN ('done', 'failed', 'cancelled') AND OLD.state IS DISTINCT FROM NEW.state THEN
        PERFORM audit_org(NEW.org_id, 'import.run', 'import', NEW.id::text, NULL,
            jsonb_build_object('source', NEW.source, 'state', NEW.state, 'created', NEW.progress, 'total', NEW.total), NEW.created_by);
    END IF;
    RETURN NULL;
END $$;
DROP TRIGGER queue_roadmap_visibility ON roadmaps;
DROP TRIGGER queue_integration_settings ON calendar_connections;
DROP TRIGGER queue_integration_settings ON org_integrations;
DROP TRIGGER queue_integration_settings ON chat_webhooks;
DROP TRIGGER queue_repo ON project_repos;
DROP TRIGGER queue_embed ON embed_keys;
DROP TRIGGER queue_embed ON embed_origins;
DROP TRIGGER queue_request_form ON projects;
DROP TRIGGER queue_board_settings ON boards;
DROP FUNCTION queue_roadmap_visibility(), queue_integration_settings(), queue_repo(), queue_embed(), queue_request_form(),
    queue_board_settings(), audit_changes(jsonb, jsonb, text[]);
