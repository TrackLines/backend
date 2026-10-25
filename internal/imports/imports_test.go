package imports

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/organizations"
)

func TestCheck(t *testing.T) {
	board := Board{SourceID: "b1", Name: "Web", Columns: []Column{{SourceID: "1", Name: "To do"}, {SourceID: "3", Name: "Done"}}}
	p := Plan{Projects: []Project{
		{SourceID: "10", Name: "Shop", Prefix: "SHOP", Boards: []Board{board}, Tickets: []Ticket{
			{SourceID: "a", Number: 4, Type: "bug", Title: "Fix", Board: "b1", Column: "1", Comments: []Comment{{Body: "hi"}}},
			{SourceID: "b", Number: 9, Type: "story", Title: "Odd type"},
			{SourceID: "c", Number: 10, Type: "task", Title: "Nowhere", Board: "b1", Column: "2"},
			{SourceID: "d", Number: 4, Type: "task", Title: "Same number"},
		}},
		{SourceID: "11", Name: "Taken", Prefix: "TL", Boards: []Board{{SourceID: "b2", Name: "Thin", Columns: []Column{{SourceID: "1", Name: "Only"}}}}},
		{SourceID: "12", Name: "Bad", Prefix: "toolongkey"},
		{SourceID: "13", Name: "Twice", Prefix: "SHOP"},
	}}
	problems, skips, n := check(p, org{projects: 2, limit: 5, prefixes: []string{"TL"}})
	codes := map[string]string{}
	for _, pr := range problems {
		codes[pr.Code+":"+pr.SourceID] = pr.Message
	}
	for _, want := range []string{"project_limit:", "prefix_taken:11", "bad_prefix:12", "prefix_taken:13", "board_columns:b2", "ticket_number:d"} {
		if _, ok := codes[want]; !ok {
			t.Errorf("missing problem %s in %v", want, problems)
		}
	}
	if len(problems) != 6 {
		t.Errorf("problems: %v", problems)
	}
	if len(skips) != 2 || skips[0].SourceID != "b" || skips[1].SourceID != "c" {
		t.Errorf("skips: %+v", skips)
	}
	if n != (Counts{Projects: 4, Boards: 2, Columns: 3, Tickets: 2, Comments: 1}) {
		t.Errorf("counts: %+v", n)
	}
	if problems, _, _ := check(Plan{Projects: p.Projects[:1]}, org{projects: 3, limit: -1}); len(problems) != 1 || problems[0].Code != "ticket_number" {
		t.Errorf("unlimited plan: %v", problems)
	}
}

// fake is a source handing back a fixed plan.
type fake struct{ plan Plan }

// Fetch: a source's attachment download (TL-540); "broken" fails.
func (f fake) Fetch(_ context.Context, _, url string, _ int64) ([]byte, error) {
	if url == "broken" {
		return nil, errors.New("gone")
	}
	return []byte("hello"), nil
}

// files is UploadThing: what was stored, by key.
type files map[string]string

func (f files) Upload(_ context.Context, name, _ string, data []byte) (string, string, error) {
	key := fmt.Sprintf("imp-%s-%d", strings.ReplaceAll(name, ".", "-"), len(f))
	f[key] = string(data)
	return key, "https://app.ufs.sh/f/" + key, nil
}

func (f fake) Plan(_ context.Context, _ string, options json.RawMessage) (Plan, error) {
	if string(options) == `"broken"` {
		return Plan{}, errors.New("bad credentials")
	}
	return f.plan, nil
}

func TestImports(t *testing.T) {
	ctx := context.Background()
	url := os.Getenv("TEST_DATABASE_URL")
	m, err := migrate.New("file://../migrations", "pgx5"+url[len("postgres"):])
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatal(err)
	}
	db, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const orgID = "im1"
	if _, err := db.Exec(ctx, `DELETE FROM imports WHERE org_id IN ('im1', 'im2'); DELETE FROM projects WHERE owner_clerk_id IN ('im1', 'im2');
		DELETE FROM org_billing WHERE org_id = 'im1'; DELETE FROM org_admins WHERE org_id IN ('im1', 'im2');
		INSERT INTO org_admins (org_id, user_clerk_id, granted_by) VALUES ('im1', 'user_admin', 'test'), ('im2', 'user_other', 'test')`); err != nil {
		t.Fatal(err)
	}
	var teamID string
	_ = db.QueryRow(ctx, `INSERT INTO teams (org_id, name) VALUES ('im1', 'Web team') RETURNING id::text`).Scan(&teamID)
	when := time.Date(2025, 3, 1, 9, 0, 0, 0, time.UTC)
	recent := time.Now().Add(-24 * time.Hour) // newer than 6 months: no review (TL-541)
	plan := Plan{Skipped: []Skip{{Kind: "ticket", SourceID: "x", Reason: "the source left it out"}}, Projects: []Project{{
		SourceID: "10000", Name: "Shop", Prefix: "SHOP",
		Boards: []Board{{SourceID: "1", Name: "Web", EstimateScale: "fibonacci", TeamID: &teamID,
			Columns: []Column{{SourceID: "1", Name: "To do"}, {SourceID: "3", Name: "In progress"}, {SourceID: "10001", Name: "Done"}}}},
		Tickets: []Ticket{
			{SourceID: "100", Number: 7, Type: "epic", Title: "Checkout", Board: "1", Column: "1"},
			{SourceID: "101", Number: 12, Type: "bug", Title: "Card declined", Description: "**boom**", Priority: "urgent", Estimate: "5",
				Labels: []string{"payments", "", strings.Repeat("x", 51)}, Board: "1", Column: "3", Parent: "100", URL: "https://acme.atlassian.net/browse/SHOP-12",
				CreatedAt: &when, UpdatedAt: &recent, Comments: []Comment{{SourceID: "c1", Author: "Old Name", Body: "seen it"}, {SourceID: "c2", Author: "x", Body: "  "}},
				BlockedBy: []string{"102", "999"}, Attachments: []Attachment{
					{SourceID: "a1", Name: "log.txt", Size: 5, Type: "text/plain", URL: "ok"},
					{SourceID: "a2", Name: "big.zip", Size: MaxCopy + 1, URL: "big", Link: "https://jira.example/big"},
					{SourceID: "a3", Name: "lost.png", Size: 5, URL: "broken"}}},
			{SourceID: "102", Number: 3, Type: "task", Title: "Sized wrong", Estimate: "4", Board: "1", Column: "10001", BlockedBy: []string{"101"}},
			{SourceID: "105", Number: 8, Type: "task", Title: "Shipped", Done: true, DoneAt: &when},
			{SourceID: "106", Number: 9, Type: "task", Title: "Forgotten", Board: "1", Column: "1", UpdatedAt: &when},
			{SourceID: "103", Number: 5, Type: "feature", Title: "Backlog idea", Parent: "999"},
			{SourceID: "104", Number: 6, Type: "story", Title: "Unknown type"},
		},
	}}}
	stored := files{}
	h := NewSystem(db, organizations.Admins{DB: db}, func() bool { return true }, map[string]Source{"fake": fake{plan}}).WithFiles(stored)
	h.spawn = func(f func()) { f() } // run right away
	as := func(user, org string) context.Context {
		return clerk.ContextWithSessionClaims(ctx, &clerk.SessionClaims{RegisteredClaims: clerk.RegisteredClaims{Subject: user}, Claims: clerk.Claims{ActiveOrganizationID: org}})
	}
	admin := as("user_admin", orgID)
	call := func(c context.Context, handler http.HandlerFunc, id, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest("POST", "/", strings.NewReader(body)).WithContext(c)
		req.Header.Set("Accept", "application/json") // coded errors
		req.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		handler(rec, req)
		return rec
	}
	preview := func(c context.Context) (int, Import) {
		rec := call(c, h.Preview, "", `{"source":"fake","options":{}}`)
		var im Import
		_ = json.Unmarshal(rec.Body.Bytes(), &im)
		return rec.Code, im
	}
	get := func(id string) Import {
		var im Import
		_ = json.Unmarshal(call(admin, h.Get, id, "").Body.Bytes(), &im)
		return im
	}

	// only admins, signed in; behind the flag; a known source
	if code, _ := preview(as("user_member", orgID)); code != 403 {
		t.Fatalf("member: %d", code)
	}
	if code, _ := preview(auth.WithAPIKeyUser(ctx, orgID, "user_admin")); code != 403 {
		t.Fatalf("API key: %d", code)
	}
	off := h
	off.on = func() bool { return false }
	if rec := call(admin, off.Preview, "", `{"source":"fake"}`); rec.Code != 404 {
		t.Fatalf("flag off: %d", rec.Code)
	}
	if rec := call(admin, h.Preview, "", `{"source":"nope"}`); rec.Code != 400 {
		t.Fatalf("unknown source: %d", rec.Code)
	}
	if rec := call(admin, h.Preview, "", `{"source":"fake","options":"broken"}`); rec.Code != 400 || !strings.Contains(rec.Body.String(), "bad credentials") {
		t.Fatalf("source error: %d %s", rec.Code, rec.Body)
	}

	// Free plan with a project already: the preview says so, and it can't run until that changes
	if _, err := db.Exec(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('im1', 'Existing')`); err != nil {
		t.Fatal(err)
	}
	code, im := preview(admin)
	if code != 201 || im.State != "previewing" || len(im.Problems) != 1 || im.Problems[0].Code != "project_limit" {
		t.Fatalf("preview: %d %+v", code, im)
	}
	if im.Planned == nil || *im.Planned != (Counts{Projects: 1, Boards: 1, Columns: 3, Tickets: 6, Comments: 2, Attachments: 2}) || len(im.Skipped) != 2 ||
		len(im.Projects) != 1 || im.Projects[0].Prefix != "SHOP" || len(im.Projects[0].Boards[0].Columns) != 3 {
		t.Fatalf("preview plan: %+v", im)
	}
	if rec := call(admin, h.Run, im.ID, ""); rec.Code != 409 || !strings.Contains(rec.Body.String(), "import_problems") {
		t.Fatalf("run with problems: %d %s", rec.Code, rec.Body)
	}
	// upgraded to Pro: the same preview now runs
	if _, err := db.Exec(ctx, `INSERT INTO org_billing (org_id, plan, stripe_status) VALUES ('im1', 'pro', 'active')`); err != nil {
		t.Fatal(err)
	}
	if got := get(im.ID); len(got.Problems) != 0 {
		t.Fatalf("after upgrading: %+v", got.Problems)
	}
	// another org can't see or run it
	if rec := call(as("user_other", "im2"), h.Get, im.ID, ""); rec.Code != 404 {
		t.Fatalf("other org get: %d", rec.Code)
	}
	if rec := call(as("user_other", "im2"), h.Run, im.ID, ""); rec.Code != 404 {
		t.Fatalf("other org run: %d", rec.Code)
	}
	if rec := call(admin, h.Run, im.ID, ""); rec.Code != 202 {
		t.Fatalf("run: %d %s", rec.Code, rec.Body)
	}
	done := get(im.ID)
	if done.State != "done" || done.Progress != 6 || done.Total != 6 || done.Report == nil ||
		done.Report.Created != (Counts{Projects: 1, Boards: 1, Columns: 3, Tickets: 6, Comments: 1, Attachments: 1}) {
		t.Fatalf("done: %+v", done)
	}
	reasons := map[string]string{}
	for _, s := range done.Report.Skipped {
		reasons[s.Kind+":"+s.SourceID] = s.Reason
	}
	for _, k := range []string{"ticket:x", "ticket:104", "comment:c2", "estimate:102", "parent:103", "blocked_by:101", "attachment:a2", "attachment:a3"} {
		if reasons[k] == "" {
			t.Errorf("report misses %s: %+v", k, done.Report.Skipped)
		}
	}
	if rec := call(admin, h.Run, im.ID, ""); rec.Code != 409 {
		t.Fatalf("run twice: %d", rec.Code)
	}

	// what it made matches the mapping: the prefix, the numbers kept, columns, team, labels, comments, the epic in the backlog
	var projectID, prefix string
	var seq int
	if err := db.QueryRow(ctx, `SELECT p.id::text, p.key_prefix, p.ticket_seq FROM projects p JOIN import_items i ON i.target_id = p.id
		WHERE i.import_id = $1 AND i.kind = 'project' AND i.source_id = '10000'`, im.ID).Scan(&projectID, &prefix, &seq); err != nil || prefix != "SHOP" || seq != 12 {
		t.Fatalf("project: %v %s %d", err, prefix, seq)
	}
	var number int
	var board, column, parent, labels, source, link, created *string
	var priority, estimate, team string
	if err := db.QueryRow(ctx, `SELECT t.number, t.board_id::text, c.name, t.parent_id::text, t.priority::text, coalesce(t.estimate, ''),
			(SELECT string_agg(label, ',') FROM ticket_labels WHERE ticket_id = t.id), t.external_source, t.external_url,
			to_char(t.created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD'), (SELECT team_id::text FROM boards WHERE id = t.board_id)
		FROM tickets t JOIN import_items i ON i.target_id = t.id LEFT JOIN columns c ON c.id = t.column_id
		WHERE i.import_id = $1 AND i.kind = 'ticket' AND i.source_id = '101'`, im.ID).
		Scan(&number, &board, &column, &parent, &priority, &estimate, &labels, &source, &link, &created, &team); err != nil {
		t.Fatal(err)
	}
	if number != 12 || board == nil || *column != "In progress" || parent == nil || priority != "urgent" || estimate != "5" ||
		*labels != "payments" || *source != "fake" || *link != "https://acme.atlassian.net/browse/SHOP-12" || *created != "2025-03-01" || team != teamID {
		t.Fatalf("SHOP-12: %d %v %s %v %s %s %s %s %s %s %s", number, board, *column, parent, priority, estimate, *labels, *source, *link, *created, team)
	}
	var epicOnBoard bool
	_ = db.QueryRow(ctx, `SELECT board_id IS NOT NULL FROM tickets WHERE project_id = $1 AND number = 7`, projectID).Scan(&epicOnBoard)
	if epicOnBoard {
		t.Fatal("an epic went on the board")
	}
	// done: in the Done column it has a done time; finished in the backlog it's resolved, when it was (TL-538)
	var inDone, shipped string
	_ = db.QueryRow(ctx, `SELECT (done_at IS NOT NULL AND resolution IS NULL)::text FROM tickets WHERE project_id = $1 AND number = 3`, projectID).Scan(&inDone)
	_ = db.QueryRow(ctx, `SELECT resolution || ' ' || to_char(done_at AT TIME ZONE 'UTC', 'YYYY-MM-DD') || ' ' || (resolved_at = done_at)::text
		FROM tickets WHERE project_id = $1 AND number = 8`, projectID).Scan(&shipped)
	if inDone != "true" || shipped != "done 2025-03-01 true" {
		t.Fatalf("done: in Done %q, resolved %q", inDone, shipped)
	}
	// blocked-by: SHOP-12 waits on SHOP-3; SHOP-3's block back on SHOP-12 would be a cycle, so it's left out
	var blocks []string
	rows, _ := db.Query(ctx, `SELECT a.number || '<' || b.number FROM ticket_dependencies d JOIN tickets a ON a.id = d.ticket_id JOIN tickets b ON b.id = d.blocked_by_id
		WHERE a.project_id = $1`, projectID)
	for rows.Next() {
		var b string
		rows.Scan(&b)
		blocks = append(blocks, b)
	}
	if len(blocks) != 1 || blocks[0] != "12<3" {
		t.Fatalf("blocked by: %v", blocks)
	}
	// review (TL-541): the open ticket untouched for over 6 months is in the backlog, set aside for its
	// board; the recent one and the one in Done (as old) stay on the board
	var forgotten, reviewBoard, staleDone string
	_ = db.QueryRow(ctx, `SELECT (t.board_id IS NULL)::text, r.board_id::text FROM tickets t JOIN import_reviews r ON r.ticket_id = t.id
		WHERE t.project_id = $1 AND t.number = 9`, projectID).Scan(&forgotten, &reviewBoard)
	_ = db.QueryRow(ctx, `SELECT count(*)::text FROM import_reviews r JOIN tickets t ON t.id = r.ticket_id WHERE t.project_id = $1`, projectID).Scan(&staleDone)
	if forgotten != "true" || board == nil || reviewBoard != *board || staleDone != "1" {
		t.Fatalf("review: in the backlog %q, for board %q (want %v), %s set aside", forgotten, reviewBoard, board, staleDone)
	}
	// attachments (TL-540): the small one copied onto the ticket, the big one linked in its description
	var fileKey, fileURL, desc string
	_ = db.QueryRow(ctx, `SELECT a.file_key, a.url, t.description FROM ticket_attachments a JOIN tickets t ON t.id = a.ticket_id
		WHERE t.project_id = $1 AND t.number = 12`, projectID).Scan(&fileKey, &fileURL, &desc)
	if stored[fileKey] != "hello" || fileURL != "https://app.ufs.sh/f/"+fileKey || len(stored) != 1 ||
		!strings.HasPrefix(desc, "**boom**\n\n**Attachments, in the original:**\n- [big.zip](https://jira.example/big)") {
		t.Fatalf("attachments: %q %q %q %v", fileKey, fileURL, desc, stored)
	}
	var author string
	_ = db.QueryRow(ctx, `SELECT c.author FROM ticket_comments c JOIN import_items i ON i.target_id = c.id WHERE i.import_id = $1 AND i.source_id = 'c1'`, im.ID).Scan(&author)
	if author != "Old Name" {
		t.Fatalf("comment author: %q", author)
	}
	if sc := get(im.ID).Scrappable; sc == nil || !*sc {
		t.Fatalf("untouched import not scrappable: %v", sc)
	}
	// a ticket made by hand afterwards carries on after the highest imported number
	var next int
	_ = db.QueryRow(ctx, `INSERT INTO tickets (project_id, type, title, position, created_by) VALUES ($1, 'task', 'new', 0, 'u') RETURNING number`, projectID).Scan(&next)
	if next != 13 {
		t.Fatalf("next number: %d", next)
	}
	if rec := call(admin, h.Scrap, im.ID, ""); rec.Code != 409 || !strings.Contains(rec.Body.String(), "import_changed") {
		t.Fatalf("scrap after adding a ticket: %d %s", rec.Code, rec.Body)
	}
	if sc := get(im.ID).Scrappable; sc == nil || *sc {
		t.Fatalf("changed import still scrappable: %v", sc)
	}
	if _, err := db.Exec(ctx, `DELETE FROM tickets_all WHERE project_id = $1 AND number = 13`, projectID); err != nil {
		t.Fatal(err)
	}

	// untouched: scrap deletes it all; another org can't
	if rec := call(as("user_other", "im2"), h.Scrap, im.ID, ""); rec.Code != 404 {
		t.Fatalf("other org scrap: %d", rec.Code)
	}
	if rec := call(admin, h.Scrap, im.ID, ""); rec.Code != 204 {
		t.Fatalf("scrap: %d %s", rec.Code, rec.Body)
	}
	var left int
	_ = db.QueryRow(ctx, `SELECT count(*) FROM projects WHERE id::text = $1`, projectID).Scan(&left)
	var queued int // the copied file goes to the sweeper with its ticket
	_ = db.QueryRow(ctx, `SELECT count(*) FROM deleted_files WHERE file_key = $1`, fileKey).Scan(&queued)
	if queued != 1 {
		t.Fatal("scrapping didn't queue the copied file for deletion")
	}
	if left != 0 || get(im.ID).State != "scrapped" {
		t.Fatalf("scrapped: %d projects, %s", left, get(im.ID).State)
	}
	if rec := call(admin, h.Scrap, im.ID, ""); rec.Code != 409 {
		t.Fatalf("scrap twice: %d", rec.Code)
	}

	// once anyone acts on an imported ticket, scrap is refused
	for _, change := range []string{
		`UPDATE tickets SET title = 'edited' WHERE id = $1`,
		`INSERT INTO ticket_comments (ticket_id, body, author) VALUES ($1, 'on it', 'user_admin')`,
		`UPDATE tickets SET assigned_to = 'user_admin' WHERE id = $1`,
		`INSERT INTO ticket_labels (ticket_id, label) VALUES ($1, 'new')`,
	} {
		_, im := preview(admin)
		if rec := call(admin, h.Run, im.ID, ""); rec.Code != 202 {
			t.Fatalf("run again: %d %s", rec.Code, rec.Body)
		}
		var ticket string
		_ = db.QueryRow(ctx, `SELECT target_id::text FROM import_items WHERE import_id = $1 AND kind = 'ticket' AND source_id = '102'`, im.ID).Scan(&ticket)
		if _, err := db.Exec(ctx, change, ticket); err != nil {
			t.Fatal(err)
		}
		if rec := call(admin, h.Scrap, im.ID, ""); rec.Code != 409 {
			t.Fatalf("scrap after %q: %d", change, rec.Code)
		}
		_, _ = db.Exec(ctx, `DELETE FROM projects WHERE id = (SELECT target_id FROM import_items WHERE import_id = $1 AND kind = 'project')`, im.ID)
	}

	// cancel: a preview is dropped; a running import stops and keeps nothing
	_, p := preview(admin)
	if rec := call(admin, h.Cancel, p.ID, ""); rec.Code != 204 || get(p.ID).State != "cancelled" {
		t.Fatalf("cancel preview: %d %s", rec.Code, get(p.ID).State)
	}
	if rec := call(admin, h.Cancel, p.ID, ""); rec.Code != 409 {
		t.Fatalf("cancel twice: %d", rec.Code)
	}
	_, r := preview(admin)
	run := h
	run.spawn = func(f func()) { // cancelled before the run gets going: it notices at its first heartbeat
		_ = h.store.Cancel(ctx, orgID, r.ID)
		f()
	}
	if rec := call(admin, run.Run, r.ID, ""); rec.Code != 202 {
		t.Fatalf("run to cancel: %d", rec.Code)
	}
	var made int
	_ = db.QueryRow(ctx, `SELECT count(*) FROM import_items WHERE import_id = $1`, r.ID).Scan(&made)
	if got := get(r.ID); got.State != "cancelled" || made != 0 {
		t.Fatalf("cancelled run: %s, %d items", got.State, made)
	}

	// a run that died with its server reads as failed once its heartbeat is old
	_, _ = db.Exec(ctx, `UPDATE imports SET state = 'running', updated_at = now() - interval '1 hour' WHERE id = $1`, r.ID)
	if got := get(r.ID); got.State != "failed" || got.Error != "interrupted" {
		t.Fatalf("stale: %+v", got)
	}

	// the list is the org's own
	var list []Import
	_ = json.Unmarshal(call(admin, h.List, "", "").Body.Bytes(), &list)
	var others []Import
	_ = json.Unmarshal(call(as("user_other", "im2"), h.List, "", "").Body.Bytes(), &others)
	if len(list) < 7 || len(others) != 0 {
		t.Fatalf("lists: %d, other org %d", len(list), len(others))
	}
}
