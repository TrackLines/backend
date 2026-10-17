-- Time away (TL-406): when people are off, from their Google Calendar Out of office events, and
-- company days off (user_id NULL) from a shared holidays calendar. Only when, never what: no titles,
-- descriptions or attendees. Goes when the org disconnects Google Calendar.
CREATE TABLE time_away (
    id        uuid PRIMARY KEY DEFAULT uuidv7(),
    org_id    text NOT NULL REFERENCES calendar_connections (org_id) ON DELETE CASCADE,
    user_id   text,                         -- NULL: a company day off, everyone
    starts_at timestamptz NOT NULL,
    ends_at   timestamptz NOT NULL,
    all_day   boolean NOT NULL DEFAULT false,
    source_id text NOT NULL,                -- the calendar event's id
    CHECK (ends_at > starts_at)
);

CREATE INDEX time_away_org_idx ON time_away (org_id, ends_at);
