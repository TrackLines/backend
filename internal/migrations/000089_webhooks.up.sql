-- Outbound webhooks (TL-635): an org's admins point events at their own https endpoints.
-- secret is sealed with the server's key (like integration tokens); secret_hint is its last 4.
CREATE TABLE webhook_endpoints (
    id                   uuid PRIMARY KEY DEFAULT uuidv7(),
    org_id               text NOT NULL,
    url                  text NOT NULL,
    events               text[] NOT NULL,
    enabled              boolean NOT NULL DEFAULT true,
    disabled_reason      text CHECK (disabled_reason IN ('failures', 'manual')),
    consecutive_failures integer NOT NULL DEFAULT 0,
    secret               bytea NOT NULL,
    secret_hint          text NOT NULL,
    created_by           text NOT NULL,
    created_at           timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX webhook_endpoints_org_idx ON webhook_endpoints (org_id);

-- The outbox (the deliveries; webhook_deliveries is inbound replay protection): one row per event per
-- subscribed endpoint, written with the change itself. The body is built from it at send time
-- ({id, event, created_at, data}), so a redelivery sends the same payload.
CREATE TABLE webhook_outbox (
    id              uuid PRIMARY KEY DEFAULT uuidv7(),
    endpoint_id     uuid NOT NULL REFERENCES webhook_endpoints (id) ON DELETE CASCADE,
    event           text NOT NULL,
    data            jsonb NOT NULL,
    state           text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'ok', 'failed')),
    attempts        integer NOT NULL DEFAULT 0,
    status_code     integer,
    error           text,
    duration_ms     integer,
    response_body   text, -- the first 2 KB
    created_at      timestamptz NOT NULL DEFAULT now(),
    next_attempt_at timestamptz DEFAULT now() -- null once it's settled
);
CREATE INDEX webhook_outbox_due_idx ON webhook_outbox (next_attempt_at) WHERE state = 'pending';
CREATE INDEX webhook_outbox_endpoint_idx ON webhook_outbox (endpoint_id, created_at DESC);
CREATE INDEX webhook_outbox_created_idx ON webhook_outbox (created_at); -- the 30-day clean-up
