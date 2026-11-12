-- TL-882: an urgent notification opens the bell / Notifications for you (once); the launch offer for now.
ALTER TABLE notifications ADD COLUMN urgent boolean NOT NULL DEFAULT false;
