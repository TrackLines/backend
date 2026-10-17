-- Google Calendar (TL-405): an org reads its people's Out of office events through our Google service
-- account, which its Workspace admin lets act for the domain (domain-wide delegation). Nothing here is
-- secret: the credential is the server's.
CREATE TABLE calendar_connections (
    org_id            text PRIMARY KEY,
    domain            text NOT NULL,                -- the Workspace domain, e.g. chewedfeed.com
    admin_email       text NOT NULL,                -- a user in the domain, to check access and read the holidays calendar as
    holidays_calendar text NOT NULL DEFAULT '',     -- optional shared calendar of company days off
    connected_by      text NOT NULL,
    connected_at      timestamptz NOT NULL DEFAULT now(),
    last_sync_at      timestamptz,
    last_sync_error   text NOT NULL DEFAULT ''
);
