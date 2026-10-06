-- 000017_sub_tickets.up.sql — T-033: native sub-tickets (parent/child links)
ALTER TABLE tickets ADD COLUMN parent_id UUID REFERENCES tickets (id) ON DELETE SET NULL;
CREATE INDEX tickets_parent_idx ON tickets (parent_id);
