-- Existing keys were created as agent keys; keep their assignment behavior.
ALTER TABLE api_keys
    ADD COLUMN kind TEXT NOT NULL DEFAULT 'ai' CHECK (kind IN ('ai', 'service'));
