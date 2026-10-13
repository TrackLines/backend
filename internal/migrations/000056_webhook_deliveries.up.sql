-- Signed git webhook bodies already handled, so a captured delivery can't be replayed to re-close
-- tickets (TL-170). Keyed by a hash of the body, which the signature covers (delivery-ID headers
-- aren't signed, so a replay could change them). Kept 180 days.
CREATE TABLE webhook_deliveries (
    provider    text NOT NULL,
    body_hash   bytea NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (provider, body_hash)
);
CREATE INDEX webhook_deliveries_age_idx ON webhook_deliveries (received_at);
