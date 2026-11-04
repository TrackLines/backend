-- flags.gg agents are per board in practice (frontend agent = Frontend board, TL-804): a board's agent
-- overrides the project's default; a ticket's own still wins
ALTER TABLE boards ADD COLUMN flags_agent_id text;
