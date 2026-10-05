CREATE TABLE users (
    id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    clerk_id               TEXT NOT NULL UNIQUE, -- unique constraint is the clerk_id index
    email                  TEXT NOT NULL,
    stripe_customer_id     TEXT UNIQUE,
    stripe_subscription_id TEXT,
    stripe_status          TEXT,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now()
);
