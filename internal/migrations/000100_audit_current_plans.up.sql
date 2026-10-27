-- The auditor learns each organization's plan from org.plan_changed (TL-676), but the trigger (000096) only
-- fires on a change: organizations already paying would stay unknown to it. Report every current paid plan
-- once. (An organization it doesn't know keeps its whole log, so nothing is lost meanwhile.)
INSERT INTO audit_outbox (org_id, actor_kind, action, target_type, target_id, data)
SELECT org_id, 'system', 'org.plan_changed', 'org', org_id, jsonb_build_object('plan', audit_plan(stripe_status, plan), 'from', 'free')
FROM org_billing
WHERE audit_plan(stripe_status, plan) <> 'free';
