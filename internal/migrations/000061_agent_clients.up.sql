-- Agent keys are named by what they are, not by hand (TL-269): the client the agent runs in, plus
-- a label for "other" agents. A new key's name is generated (claude-482193) and is only the id its
-- work is recorded under; people see "Claude", and everyone else "Claude (keloran)".
ALTER TABLE api_keys ADD COLUMN client text CHECK (client IN ('claude-code', 'claude-desktop', 'codex', 'cursor', 'vscode', 'gemini', 'other'));
ALTER TABLE api_keys ADD COLUMN label text;

-- existing agent keys: the client their name points to, else "other" labelled with their old name.
-- Names stay: tickets, comments and claims already record them.
UPDATE api_keys SET client = CASE
    WHEN lower(name) LIKE '%claude%desktop%' THEN 'claude-desktop'
    WHEN lower(name) LIKE '%claude%' THEN 'claude-code'
    WHEN lower(name) LIKE '%codex%' THEN 'codex'
    WHEN lower(name) LIKE '%cursor%' THEN 'cursor'
    WHEN lower(name) LIKE '%copilot%' OR lower(name) LIKE '%vscode%' OR lower(name) LIKE '%vs code%' THEN 'vscode'
    WHEN lower(name) LIKE '%gemini%' THEN 'gemini'
    ELSE 'other' END
WHERE kind = 'ai';
UPDATE api_keys SET label = name WHERE kind = 'ai' AND client = 'other';
