-- Free-form labels (e.g. "agent:checkout-api" from BugFixes); a label exists while a ticket uses it.
CREATE TABLE ticket_labels (
    ticket_id UUID NOT NULL REFERENCES tickets (id) ON DELETE CASCADE,
    label     TEXT NOT NULL CHECK (label <> '' AND char_length(label) <= 50)
);
CREATE UNIQUE INDEX ticket_labels_ticket_label_idx ON ticket_labels (ticket_id, lower(label));
CREATE INDEX ticket_labels_label_idx ON ticket_labels (lower(label));
