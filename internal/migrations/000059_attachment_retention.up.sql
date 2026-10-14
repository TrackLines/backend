-- Old attachments are cleaned up (TL-259): a year after it was added (or last kept), a file is
-- warned about; unless someone keeps it, it's deleted at 13 months (and never sooner than 28 days
-- after the warning). Keeping it starts the year again.
ALTER TABLE ticket_attachments ADD COLUMN kept_at timestamptz;
ALTER TABLE ticket_attachments ADD COLUMN warned_at timestamptz;
