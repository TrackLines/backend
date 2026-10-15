-- Plans by tier (TL-290): Free 1 project, Pro 5, Enterprise unlimited. plan is the paid tier an org's
-- subscription is on, from its Stripe price. Every live plan today was unlimited (ChewedFeed's is
-- granted by hand), so they're Enterprise: nobody loses projects.
ALTER TABLE org_billing ADD COLUMN plan text CHECK (plan IN ('pro', 'enterprise'));
UPDATE org_billing SET plan = 'enterprise' WHERE stripe_status IN ('active', 'trialing');
