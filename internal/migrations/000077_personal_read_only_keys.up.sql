-- Personal keys (TL-450): act exactly as their owner (work recorded as them, assignee=me is them),
-- for the owner's own tools and displays. Any key can be read-only: GET only, nothing changed.
ALTER TABLE api_keys DROP CONSTRAINT api_keys_kind_check;
ALTER TABLE api_keys ADD CONSTRAINT api_keys_kind_check CHECK (kind IN ('ai', 'service', 'personal'));
ALTER TABLE api_keys ADD COLUMN read_only boolean NOT NULL DEFAULT false;
