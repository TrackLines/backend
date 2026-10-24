-- A user's language (TL-551): a BCP 47 tag the app is translated into, or null to follow the browser.
ALTER TABLE users ADD COLUMN language text;
