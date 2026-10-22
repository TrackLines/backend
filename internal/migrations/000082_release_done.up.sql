-- Finish on release (TL-531): on a board with a Dev complete column, a merged GitHub PR moves its
-- ticket there and a published release that contains the merge moves it to Done. Null: a merge
-- finishes the ticket, as before.
ALTER TABLE boards ADD COLUMN dev_complete_column_id uuid REFERENCES columns (id) ON DELETE SET NULL;

-- merge_sha: the PR's merge commit, which a release's tag must contain. released_in: that tag.
ALTER TABLE ticket_prs
    ADD COLUMN merge_sha   text,
    ADD COLUMN released_in text,
    ADD COLUMN released_at timestamptz;
CREATE INDEX ticket_prs_unreleased_idx ON ticket_prs (provider, repo) WHERE merge_sha IS NOT NULL AND released_at IS NULL;
