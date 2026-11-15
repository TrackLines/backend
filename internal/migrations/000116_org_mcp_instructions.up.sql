-- TL-899: an organization's own additional instructions for agents, appended to the MCP pre-prompt for its
-- sessions only. No row (or an empty body): nothing is added. The global pre-prompt stays in mcp_prompts.
CREATE TABLE org_mcp_instructions (
    org_id     text PRIMARY KEY,
    body       text NOT NULL CHECK (char_length(body) <= 4000),
    updated_by text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- the org log: who changed them and how long they are now, never the text
CREATE FUNCTION queue_org_mcp_instructions() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM audit_org(NEW.org_id, 'mcp_instructions.changed', 'org', NEW.org_id, NULL,
        jsonb_build_object('length', char_length(NEW.body), 'cleared', NEW.body = ''), NEW.updated_by);
    RETURN NULL;
END $$;
CREATE TRIGGER queue_org_mcp_instructions AFTER INSERT OR UPDATE ON org_mcp_instructions FOR EACH ROW EXECUTE FUNCTION queue_org_mcp_instructions();
