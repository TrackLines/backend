-- Tickets that deliver a roadmap item; an item's progress is how many of them are done.
CREATE TABLE roadmap_item_tickets (
    item_id   UUID NOT NULL REFERENCES roadmap_items (id) ON DELETE CASCADE,
    ticket_id UUID NOT NULL REFERENCES tickets (id) ON DELETE CASCADE,
    PRIMARY KEY (item_id, ticket_id)
);
CREATE INDEX roadmap_item_tickets_ticket_idx ON roadmap_item_tickets (ticket_id);
