-- Google Calendar (audit, 2026-10-17): a Workspace domain belongs to one organization. Our service
-- account is shared by every organization, so without this one org could connect another company's
-- domain once that company had delegated to us.
CREATE UNIQUE INDEX calendar_connections_domain_key ON calendar_connections (lower(domain));

-- sprint_tickets.ticket_id cascades from tickets_all: index it so purging a ticket doesn't scan the table
CREATE INDEX sprint_tickets_ticket_idx ON sprint_tickets (ticket_id);

-- Google Chat link codes say which Google account made them, so the link page can ask "link <who>?"
-- before linking (audit: someone else's code opened by a signed-in person linked them silently)
ALTER TABLE chat_link_codes ADD COLUMN google_label text NOT NULL DEFAULT '';
