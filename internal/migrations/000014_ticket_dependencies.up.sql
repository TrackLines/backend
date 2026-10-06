-- "ticket_id can't start until blocked_by_id is done". Same-project + no-cycles are enforced in the API.
CREATE TABLE ticket_dependencies (
    ticket_id     UUID NOT NULL REFERENCES tickets (id) ON DELETE CASCADE,
    blocked_by_id UUID NOT NULL REFERENCES tickets (id) ON DELETE CASCADE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (ticket_id, blocked_by_id),
    CHECK (ticket_id <> blocked_by_id)
);
CREATE INDEX ticket_dependencies_blocker_idx ON ticket_dependencies (blocked_by_id);
