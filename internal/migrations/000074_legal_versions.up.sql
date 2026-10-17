-- Terms and Privacy history (TL-424): notices that aren't about an organization's work (a new version
-- of the Terms) go to everyone, in whichever organization they're looking at: org_id '' is all of
-- them. link is where a notice opens when it isn't a ticket or board.
ALTER TABLE notifications ADD COLUMN link text;

-- which published versions everyone has been told about, so each is announced once
CREATE TABLE legal_announcements (
    doc          text NOT NULL CHECK (doc IN ('terms', 'privacy')),
    version      text NOT NULL, -- the date it took effect, 2026-10-17
    announced_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (doc, version)
);
