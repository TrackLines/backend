-- TL-427: the member cap last set in Clerk per organization, so a start only tells Clerk about the
-- organizations whose cap changed, not every one on every deploy.
CREATE TABLE member_caps (
    org_id TEXT PRIMARY KEY,
    cap BIGINT NOT NULL,
    synced_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
