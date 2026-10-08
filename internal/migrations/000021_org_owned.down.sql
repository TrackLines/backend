-- Org-owned rows can't point back at users, so the user foreign keys are not restored.
DROP INDEX IF EXISTS api_keys_org_idx;
ALTER TABLE api_keys DROP COLUMN org_id;
