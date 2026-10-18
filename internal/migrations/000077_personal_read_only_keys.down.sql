DELETE FROM api_keys WHERE kind = 'personal';
ALTER TABLE api_keys DROP COLUMN read_only;
ALTER TABLE api_keys DROP CONSTRAINT api_keys_kind_check;
ALTER TABLE api_keys ADD CONSTRAINT api_keys_kind_check CHECK (kind IN ('ai', 'service'));
