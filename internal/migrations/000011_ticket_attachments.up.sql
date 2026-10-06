-- Files attached to tickets. Bytes live on UploadThing; we keep the key (for deletion) and URL.
CREATE TABLE ticket_attachments (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    ticket_id    UUID NOT NULL REFERENCES tickets (id) ON DELETE CASCADE,
    file_key     TEXT NOT NULL UNIQUE,
    url          TEXT NOT NULL,
    name         TEXT NOT NULL,
    size_bytes   BIGINT NOT NULL DEFAULT 0,
    content_type TEXT NOT NULL DEFAULT '',
    created_by   TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ticket_attachments_ticket_idx ON ticket_attachments (ticket_id, created_at);
