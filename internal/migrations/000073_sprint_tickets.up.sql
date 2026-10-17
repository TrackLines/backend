-- Sprint reports (TL-397): which tickets a sprint held when it started and when it closed, so a report
-- can say what was planned, what was added or removed along the way, what got done and what carried
-- over. Estimates and types are copied in, as they were then. Sprints before this have no rows.
CREATE TABLE sprint_tickets (
    sprint_id uuid NOT NULL REFERENCES sprints (id) ON DELETE CASCADE,
    ticket_id uuid NOT NULL REFERENCES tickets_all (id) ON DELETE CASCADE,
    at_start  boolean NOT NULL DEFAULT false,
    at_close  boolean NOT NULL DEFAULT false,
    done      boolean NOT NULL DEFAULT false, -- finished when the sprint closed
    estimate  text,
    type      text NOT NULL,
    PRIMARY KEY (sprint_id, ticket_id)
);

ALTER TABLE sprints
    ADD COLUMN snapshot      boolean NOT NULL DEFAULT false, -- its starting tickets were recorded
    ADD COLUMN approved_over numeric;                       -- the over-capacity total a lead approved for it
