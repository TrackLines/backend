package boards

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/organizations"
)

// The Kanban flow report (TL-621): WIP per column (Done left out, empty columns kept), work mix,
// per-week cycle time, and the same as Markdown.
func TestFlowReport(t *testing.T) {
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
	const org, other = "org_flow1", "org_flow2"
	for _, q := range []string{`DELETE FROM boards WHERE owner_clerk_id IN ($1, $2)`, `DELETE FROM projects WHERE owner_clerk_id IN ($1, $2)`} {
		if _, err := db.Exec(ctx, q, org, other); err != nil {
			t.Fatal(err)
		}
	}
	s := Store{DB: db}
	var pid string
	if err := db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ($1, 'Flow') RETURNING id`, org).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateFrom(ctx, org, pid, "Team \"A\": ops|x", "", Builtins[2]) // Ready, In progress, Review, Done
	if err != nil {
		t.Fatal(err)
	}
	ready, doing, review, done := b.Columns[0].ID, b.Columns[1].ID, b.Columns[2].ID, b.Columns[3].ID
	// a column name that would break a table or a chart label
	if _, err := db.Exec(ctx, `UPDATE columns SET name = $2 WHERE id = $1`, doing, "Doing: \"now\"\nreally|"); err != nil {
		t.Fatal(err)
	}
	for q, col := range map[string]string{
		`INSERT INTO tickets (project_id, board_id, column_id, title, position) VALUES ($1, $2, $3, 'open 1', 0), ($1, $2, $3, 'open 2', 1),
			($1, $2, $3, 'resolved', 2), ($1, $2, $3, 'duplicate', 3)`: ready,
		`INSERT INTO tickets (project_id, board_id, column_id, title, position) VALUES ($1, $2, $3, 'doing', 0)`: doing,
		`INSERT INTO tickets (project_id, board_id, column_id, title, position, type, started_at, done_at) VALUES
			($1, $2, $3, 'b1', 0, 'bug', now() - interval '2 days', now()),
			($1, $2, $3, 'b2', 1, 'bug', now() - interval '4 days', now()),
			($1, $2, $3, 'f1', 2, 'feature', now() - interval '6 days', now()),
			($1, $2, $3, 't1', 3, 'task', NULL, now())`: done,
	} {
		if _, err := db.Exec(ctx, q, pid, b.ID, col); err != nil {
			t.Fatal(err)
		}
	}
	// closed without leaving the column: not open work
	for _, q := range []string{
		`UPDATE tickets SET resolved_at = now(), resolution = 'wont_do' WHERE board_id = $1 AND title = 'resolved'`,
		`UPDATE tickets SET duplicate_of = (SELECT id FROM tickets WHERE board_id = $1 AND title = 'open 1') WHERE board_id = $1 AND title = 'duplicate'`,
	} {
		if _, err := db.Exec(ctx, q, b.ID); err != nil {
			t.Fatal(err)
		}
	}

	f, err := s.Flow(ctx, org, b.ID, 4)
	if err != nil {
		t.Fatal(err)
	}
	wantWIP := []ColumnWIP{{ready, "Ready", 2}, {doing, "Doing: \"now\"\nreally|", 1}, {review, "Review", 0}}
	if len(f.WIP) != len(wantWIP) {
		t.Fatalf("wip: %+v", f.WIP)
	}
	for i := range wantWIP {
		if f.WIP[i] != wantWIP[i] {
			t.Fatalf("wip[%d] = %+v, want %+v", i, f.WIP[i], wantWIP[i])
		}
	}
	wantMix := []TypeCount{{"bug", 2}, {"feature", 1}, {"task", 1}}
	if len(f.Mix) != len(wantMix) {
		t.Fatalf("mix: %+v", f.Mix)
	}
	for i := range wantMix {
		if f.Mix[i] != wantMix[i] {
			t.Fatalf("mix[%d] = %+v, want %+v", i, f.Mix[i], wantMix[i])
		}
	}
	this := f.Throughput[3]
	if this.Tickets != 4 || this.MedianCycleDays == nil || num(*this.MedianCycleDays) != "4" || num(*this.P85CycleDays) != "5.4" {
		t.Fatalf("this week: %+v", this)
	}
	if f.Throughput[0].MedianCycleDays != nil {
		t.Fatalf("an empty week has no cycle time: %+v", f.Throughput[0])
	}

	h := NewSystem(db, organizations.Admins{DB: db})
	get := func(o, query string) *httptest.ResponseRecorder {
		c := &clerk.SessionClaims{RegisteredClaims: clerk.RegisteredClaims{Subject: "member-u"}}
		c.ActiveOrganizationID = o
		req := httptest.NewRequest("GET", "/?"+query, nil).WithContext(clerk.ContextWithSessionClaims(ctx, c))
		req.SetPathValue("id", b.ID)
		rec := httptest.NewRecorder()
		h.Flow(rec, req)
		return rec
	}
	var body map[string]json.RawMessage
	if rec := get(org, "weeks=4"); rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &body) != nil || body["wip"] == nil || body["mix"] == nil || body["board_name"] != nil {
		t.Fatalf("json: %d %s", rec.Code, rec.Body)
	}
	if rec := get(other, "format=md"); rec.Code != 404 {
		t.Fatalf("another org's board: %d", rec.Code)
	}

	rec := get(org, "weeks=4&format=md")
	md := rec.Body.String()
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "text/markdown; charset=utf-8" || rec.Header().Get("X-Content-Type-Options") != "nosniff" ||
		rec.Header().Get("Content-Disposition") != `attachment; filename="team-a-ops-x-flow-`+time.Now().UTC().Format(time.DateOnly)+`.md"` {
		t.Fatalf("markdown headers: %d %v", rec.Code, rec.Header())
	}
	for _, want := range []string{
		`# Flow report: Team "A": ops\|x`,
		"| Tickets done | Median cycle time (days) | 85th percentile cycle time (days) |\n| --- | --- | --- |\n| 4 | 4 | 5.4 |",
		"xychart-beta\n    title \"Tickets done per week\"", "    bar [0, 0, 0, 4]", "| " + f.Throughput[3].WeekStart + " | 4 |",
		"    line [0, 0, 0, 4]\n    line [0, 0, 0, 5.4]", "| " + f.Throughput[0].WeekStart + " | – | – |", "| " + f.Throughput[3].WeekStart + " | 4 | 5.4 |",
		"pie title Open tickets by column\n    \"Ready\" : 2\n    \"Doing: 'now' really|\" : 1\n```", // Review (0) has no slice
		"| Ready | 2 |", `| Doing: "now" really\| | 1 |`, "| Review | 0 |",
		"pie title Tickets done by type\n    \"Bug\" : 2\n    \"Feature\" : 1\n    \"Task\" : 1\n```", "| Bug | 2 |",
	} {
		if !strings.Contains(md, want) {
			t.Fatalf("markdown misses %q:\n%s", want, md)
		}
	}

	// an empty board: no pies at all (every value zero), tables still there
	empty, _ := s.CreateFrom(ctx, org, pid, "Quiet", "", Builtins[2])
	f, err = s.Flow(ctx, org, empty.ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if md := FlowMarkdown(f, time.UTC, time.Now()); strings.Contains(md, "pie") || !strings.Contains(md, "| Review | 0 |") || len(f.Mix) != 0 || f.Mix == nil {
		t.Fatalf("empty board: %+v\n%s", f.Mix, md)
	}
}
