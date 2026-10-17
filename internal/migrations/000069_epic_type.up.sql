-- Epics (TL-213): a ticket type for large pieces of work. They live in the backlog, never on a board or
-- in a sprint, take no estimate, and get their progress and size from their sub-tickets.
ALTER TYPE ticket_type ADD VALUE IF NOT EXISTS 'epic';
