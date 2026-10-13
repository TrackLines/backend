-- The plan belongs to the organization (TL-243): one Stripe customer and subscription per org, paid
-- for by an org admin. stripe_status set by hand with no subscription = a plan granted by hand.
CREATE TABLE org_billing (
    org_id                 TEXT PRIMARY KEY,
    stripe_customer_id     TEXT UNIQUE,
    stripe_subscription_id TEXT,
    stripe_status          TEXT,
    billing_email          TEXT NOT NULL DEFAULT '',
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- plans paid per person until now move to the first org that person is an admin of, so nobody
-- loses Pro; the users columns stay, unused
INSERT INTO org_billing (org_id, stripe_customer_id, stripe_subscription_id, stripe_status, billing_email)
SELECT DISTINCT ON (u.clerk_id) a.org_id, u.stripe_customer_id, u.stripe_subscription_id, u.stripe_status, u.email
FROM users u JOIN org_admins a ON a.user_clerk_id = u.clerk_id
WHERE u.stripe_status IN ('active', 'trialing')
ORDER BY u.clerk_id, a.created_at
ON CONFLICT DO NOTHING;
