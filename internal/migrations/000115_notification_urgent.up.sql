-- TL-882: an urgent notification opens the bell / Notifications for you, once: shown_at is when a client
-- did, so the website and apps don't open it again (no device storage, TL-595).
ALTER TABLE notifications ADD COLUMN urgent boolean NOT NULL DEFAULT false;
ALTER TABLE notifications ADD COLUMN shown_at timestamptz;
