-- Short links (TL-277): each Tracklines link shortened once through 1tn and reused, so copying the
-- same ticket's link again gives the same short link. Links hold only ids, nothing private.
CREATE TABLE short_links (
    url        text PRIMARY KEY,
    short      text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
