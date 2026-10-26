-- TL-576: the downtime resolver, phase 1. Staff record outages; one over an hour makes every UTC day it
-- touched free for every paying organization, credited in Stripe once per (org, day), ever.
CREATE TABLE outages (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    starts_at  TIMESTAMPTZ NOT NULL,
    ends_at    TIMESTAMPTZ NOT NULL CHECK (ends_at > starts_at),
    note       TEXT NOT NULL DEFAULT '',
    created_by TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE downtime_credits (
    org_id     TEXT NOT NULL,
    day        DATE NOT NULL,
    outage_id  UUID NOT NULL REFERENCES outages (id),
    amount     BIGINT NOT NULL, -- minor units, credited (positive)
    currency   TEXT NOT NULL,
    stripe_id  TEXT NOT NULL DEFAULT '', -- the balance transaction; '' while it's being made
    created_by TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, day)
);
