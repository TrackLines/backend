-- Imported tickets waiting for a decision (TL-541): open tickets an import set aside in the backlog
-- because nothing had happened to them in the source for over 6 months. Until each is kept (its row
-- goes), closed as won't do or put on a board, their board can't start or plan a sprint.
CREATE TABLE import_reviews (
    ticket_id  UUID PRIMARY KEY REFERENCES tickets_all (id) ON DELETE CASCADE,
    board_id   UUID NOT NULL REFERENCES boards (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX import_reviews_board_idx ON import_reviews (board_id);
