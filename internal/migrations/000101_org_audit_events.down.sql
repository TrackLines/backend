DROP TRIGGER queue_org_deleted ON deleted_orgs;
DROP TRIGGER queue_import ON imports;
DROP TRIGGER queue_integration ON chat_webhooks;
DROP TRIGGER queue_integration ON linear_connections;
DROP TRIGGER queue_integration ON jira_connections;
DROP TRIGGER queue_integration ON calendar_connections;
DROP TRIGGER queue_integration ON org_integrations;
DROP TRIGGER queue_webhook ON webhook_endpoints;
DROP TRIGGER queue_org_admin ON org_admins;
DROP TRIGGER queue_invitation ON invitations;
DROP TRIGGER queue_api_key ON api_keys;
DROP TRIGGER queue_sprint ON sprints;
DROP TRIGGER queue_team_member ON team_members;
DROP TRIGGER queue_team ON teams;
DROP TRIGGER queue_board ON boards;
DROP TRIGGER queue_project ON projects;
DROP FUNCTION queue_org_deleted(), queue_import(), queue_integration(), queue_webhook(), audit_host(text),
    queue_org_admin(), queue_invitation(), queue_api_key(), queue_sprint(), queue_team_member(), queue_team(),
    queue_board(), queue_project(), audit_org(text, text, text, text, uuid, jsonb, text);
