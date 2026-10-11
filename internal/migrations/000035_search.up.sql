-- Search: weighted full-text vectors kept up to date by Postgres (title A, description B; comment
-- bodies on their own table), with GIN indexes. 'simple' keeps exact words (code, names, ids)
-- that English stemming would mangle; prefix queries (word:*) give type-ahead.
ALTER TABLE tickets ADD COLUMN search tsvector GENERATED ALWAYS AS (
    setweight(to_tsvector('simple', coalesce(title, '')), 'A') ||
    setweight(to_tsvector('simple', coalesce(description, '')), 'B')) STORED;
CREATE INDEX tickets_search_idx ON tickets USING GIN (search);

ALTER TABLE ticket_comments ADD COLUMN search tsvector GENERATED ALWAYS AS (to_tsvector('simple', body)) STORED;
CREATE INDEX ticket_comments_search_idx ON ticket_comments USING GIN (search);
