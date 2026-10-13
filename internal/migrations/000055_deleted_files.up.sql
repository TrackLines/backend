-- Files whose attachment is gone, by any path (removed by hand, or with its ticket, project or
-- organization), waiting to be deleted from UploadThing by the sweeper (TL-206).
CREATE TABLE deleted_files (
    file_key   text PRIMARY KEY,
    deleted_at timestamptz NOT NULL DEFAULT now(),
    attempts   integer NOT NULL DEFAULT 0
);

CREATE FUNCTION queue_deleted_file() RETURNS trigger AS $$
BEGIN
    INSERT INTO deleted_files (file_key) VALUES (OLD.file_key) ON CONFLICT DO NOTHING;
    RETURN OLD;
END $$ LANGUAGE plpgsql;
CREATE TRIGGER ticket_attachments_queue_file AFTER DELETE ON ticket_attachments
    FOR EACH ROW EXECUTE FUNCTION queue_deleted_file();

-- files already orphaned before this: attachments whose ticket is gone can't exist (cascade), so
-- nothing to backfill here; files left on UploadThing earlier need a one-off clean-up there

-- Organizations deleted in Clerk. Their data is kept 30 days (an accidental deletion can be undone by
-- removing the row), then purged for good, files included (TL-206).
CREATE TABLE deleted_orgs (
    org_id     text PRIMARY KEY,
    deleted_at timestamptz NOT NULL DEFAULT now()
);
