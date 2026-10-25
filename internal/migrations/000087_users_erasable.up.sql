-- A deleted person's users row is erased (TL-594, UK GDPR). API keys keep their owner's id but no
-- longer cascade from users: revoked keys (and an org's service keys an admin made) stay on record.
ALTER TABLE api_keys DROP CONSTRAINT IF EXISTS api_keys_owner_clerk_id_fkey;
