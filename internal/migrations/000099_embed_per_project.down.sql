CREATE INDEX embed_keys_org_idx ON embed_keys (org);
DROP INDEX embed_keys_project_idx;
ALTER TABLE embed_keys DROP COLUMN project_id;
CREATE INDEX embed_origins_org_idx ON embed_origins (org);
ALTER TABLE embed_origins DROP CONSTRAINT embed_origins_project_origin_key;
-- back to one list per org: keep the first copy of each origin
DELETE FROM embed_origins o USING embed_origins d WHERE o.org = d.org AND o.origin = d.origin AND o.id > d.id;
ALTER TABLE embed_origins DROP COLUMN project_id;
ALTER TABLE embed_origins ADD CONSTRAINT embed_origins_org_origin_key UNIQUE (org, origin);
