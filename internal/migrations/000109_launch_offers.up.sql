-- TL-774: the launch offer, a free year of Pro. Staff send it to chosen organizations (offered_at);
-- an admin of one claims it, once per organization ever. The row stays after the trial ends or is
-- cancelled, so the same org can't claim again.
CREATE TABLE launch_offers (
    org_id          text PRIMARY KEY,
    offered_at      timestamptz NOT NULL DEFAULT now(),
    offered_by      text NOT NULL,
    claimed_at      timestamptz,
    claimed_by      text,
    subscription_id text,        -- set once Stripe has made the trial
    trial_ends_at   timestamptz,
    reminded_30_at  timestamptz, -- the price-increase promise: told 30 days before it ends
    reminded_7_at   timestamptz  -- and again 7 days before
);
