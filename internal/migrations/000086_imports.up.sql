-- Imports (TL-536): one row per run, from preview to done (or failed, cancelled, scrapped). plan is
-- what the source (Jira, Linear…) said to create; report is what was created and what was skipped.
CREATE TABLE imports (
    id          uuid PRIMARY KEY DEFAULT uuidv7(),
    org_id      text NOT NULL,
    source      text NOT NULL,
    state       text NOT NULL DEFAULT 'previewing'
                CHECK (state IN ('previewing', 'running', 'cancelling', 'done', 'failed', 'cancelled', 'scrapped')),
    plan        jsonb NOT NULL,
    report      jsonb,
    progress    integer NOT NULL DEFAULT 0, -- tickets created so far, of total
    total       integer NOT NULL DEFAULT 0,
    error       text NOT NULL DEFAULT '',
    created_by  text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(), -- a running import's heartbeat
    started_at  timestamptz,
    finished_at timestamptz
);
CREATE INDEX imports_org_idx ON imports (org_id, created_at DESC);

-- What each import created: source id → TrackLin.es id, for scrapping it and for later imports.
-- fingerprint (tickets): the ticket as imported; any difference means someone has acted on it.
CREATE TABLE import_items (
    import_id   uuid NOT NULL REFERENCES imports (id) ON DELETE CASCADE,
    kind        text NOT NULL CHECK (kind IN ('project', 'board', 'column', 'ticket', 'comment', 'attachment')),
    source_id   text NOT NULL,
    target_id   uuid NOT NULL,
    fingerprint text,
    PRIMARY KEY (import_id, kind, source_id)
);

-- Imports keep the source's ticket numbers (TL-536): a ticket inserted with a number keeps it, and the
-- project's sequence carries on after the highest; without one it's numbered as before.
CREATE OR REPLACE FUNCTION tickets_assign_number() RETURNS trigger AS $$
BEGIN
    IF NEW.number IS NULL THEN
        UPDATE projects SET ticket_seq = ticket_seq + 1 WHERE id = NEW.project_id RETURNING ticket_seq INTO NEW.number;
    ELSE
        UPDATE projects SET ticket_seq = greatest(ticket_seq, NEW.number) WHERE id = NEW.project_id;
    END IF;
    RETURN NEW;
END $$ LANGUAGE plpgsql;
