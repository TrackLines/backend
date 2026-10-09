DROP INDEX IF EXISTS boards_team_idx;
ALTER TABLE boards DROP COLUMN team_id;
ALTER TABLE team_members DROP COLUMN leader;
DROP TABLE org_admins;
