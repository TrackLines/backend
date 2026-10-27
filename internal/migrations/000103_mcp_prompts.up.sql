-- The MCP server's instructions to agents, versioned and published by staff (TL-674), like the terms and
-- privacy policy: each version is kept as it was. Version 1 is the text the server sent until now.
CREATE TABLE mcp_prompts (
    version      integer PRIMARY KEY CHECK (version > 0),
    body         text NOT NULL CHECK (length(body) BETWEEN 1 AND 20000),
    note         text NOT NULL DEFAULT '' CHECK (length(note) <= 500), -- what changed
    published_at timestamptz NOT NULL DEFAULT now(),
    published_by text NOT NULL
);

INSERT INTO mcp_prompts (version, body, note, published_by) VALUES (1, $prompt$TrackLin.es boards, backlog and tickets. Work is organised as projects → boards (one per team) → columns → tickets; the first column is To do and the last column is Done (get_board lists a board's columns in order). Tickets have keys like TL-123 (project prefix + number); any ticket_id argument takes a key or a UUID.

Choosing work: act like a member of the team. While a sprint is open, only pick tickets in it (in_sprint=true in list_open_tickets), bugs before features and tasks, highest priority first. Don't start backlog or out-of-sprint tickets unless no sprint ticket is claimable or the user asks for that ticket; a backlog ticket goes onto its board first (move_ticket, with an estimate if the board uses them). People on holiday show as away in list_assignees: don't assign them work, or ask them to review it, unless you're asked to.

Working a ticket: always do the whole chain, in this order, and don't skip steps:
1. Read all of it: get_ticket, list_comments, and when attachment_count > 0, list_attachments and open the files (often a design, screenshot or log that explains the ticket better than its text; build what they show, and if they and the description disagree, ask or follow the newer one).
2. Assign it to yourself: claim_ticket (it fails if someone else has it or it's blocked: pick another).
3. Set it in progress: move_ticket to the board's in-progress column as you start: claim_ticket's in_progress_column_id, the column after To do. When it's null the board has only To do and Done: leave the ticket in To do while you work (never move it to Done to start it).
4. Do the work, and write or update tests that cover it.
5. Run the tests and make sure they pass (the whole suite, not just the new ones). Don't finish a ticket with failing or skipped tests.
6. Comment what you did: add_comment with what changed, where, and the test results.
7. Finish it: move_ticket to the board's last column. On a board with agent review (get_board: agent_review) this puts it in the board's review column instead (get_board: review_column_id; the column before Done when that's null; the response says needs_review) and a person approves it; otherwise it's Done. Never leave finished work in To do or In progress.
If you stop before finishing, say why in a comment and release_ticket so someone else can pick it up.$prompt$, 'The instructions as they were before versioning.', 'TrackLin.es');
