package sprints

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
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/boards"
	"github.com/tracklines/backend/internal/organizations"
	"github.com/tracklines/backend/internal/tickets"
)

// TL-398: a project's sprint boards together, and every project in the organization: points with
// points, tickets with tickets, predictability across both, by calendar month.
func TestRollups(t *testing.T) {
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
	for _, q := range []string{
		`DELETE FROM boards WHERE owner_clerk_id IN ('ru_org', 'ru_other')`,
		`DELETE FROM projects WHERE owner_clerk_id IN ('ru_org', 'ru_other')`,
		`DELETE FROM org_admins WHERE org_id = 'ru_org'`,
		`INSERT INTO org_admins (org_id, user_clerk_id, granted_by) VALUES ('ru_org', 'ru_admin', 'test')`,
	} {
		if _, err := db.Exec(ctx, q); err != nil {
			t.Fatal(q, err)
		}
	}
	bs, ts, s := boards.Store{DB: db}, tickets.Store{DB: db}, Store{DB: db}
	project := func(owner, name string) string {
		var id string
		db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ($1, $2) RETURNING id`, owner, name).Scan(&id)
		return id
	}
	// sprint: a closed, recorded sprint on board b, closed at, with tickets (estimate, done) it started with
	sprint := func(owner, board, closed, goal string, list ...[2]string) {
		var id string
		at, _ := time.Parse(time.DateOnly, closed)
		if err := db.QueryRow(ctx, `INSERT INTO sprints (board_id, number, length_days, starts_at, ends_at, closed_at, snapshot, goal)
			VALUES ($1, COALESCE((SELECT max(number) FROM sprints WHERE board_id = $1), 0) + 1, 14, $2::timestamptz - interval '14 days', $2, $2, true, $3) RETURNING id`, board, at, goal).Scan(&id); err != nil {
			t.Fatal(err)
		}
		b, _ := bs.Get(ctx, board, owner)
		for _, tk := range list {
			ticket, err := ts.Create(ctx, owner, b.Columns[0].ID, "task", "t", "")
			if err != nil {
				t.Fatal(err)
			}
			db.Exec(ctx, `INSERT INTO sprint_tickets (sprint_id, ticket_id, at_start, at_close, done, estimate, type) VALUES ($1, $2, true, true, $3, $4, 'task')`, id, ticket.ID, tk[1] == "done", tk[0])
		}
	}
	scale := func(board, scale string) {
		db.Exec(ctx, `UPDATE boards SET estimate_scale = $2 WHERE id = $1`, board, scale)
	}

	p1 := project("ru_org", "Shop")
	a, _ := bs.Create(ctx, "ru_org", p1, "Backend", "") // points
	scale(a.ID, "fibonacci")
	sprint("ru_org", a.ID, "2026-08-10", "Ship search", [2]string{"3", "done"}, [2]string{"5", "done"})
	b, _ := bs.Create(ctx, "ru_org", p1, "Design", "") // counts tickets
	scale(b.ID, "none")
	sprint("ru_org", b.ID, "2026-09-05", "New logo", [2]string{"", "done"}, [2]string{"", "open"})
	archived, _ := bs.Create(ctx, "ru_org", p1, "Old", "") // archived: left out
	sprint("ru_org", archived.ID, "2026-09-01", "", [2]string{"8", "open"})
	db.Exec(ctx, `UPDATE boards SET archived_at = now() WHERE id = $1`, archived.ID)
	kanban, _ := bs.Create(ctx, "ru_org", p1, "Support", "") // kanban: no sprints to report
	db.Exec(ctx, `UPDATE boards SET style = 'kanban' WHERE id = $1`, kanban.ID)

	p2 := project("ru_org", "Website")
	e, _ := bs.Create(ctx, "ru_org", p2, "Web", "")
	scale(e.ID, "fibonacci")
	sprint("ru_org", e.ID, "2026-09-20", "", [2]string{"2", "done"})

	pOther := project("ru_other", "Theirs") // another organization's
	o, _ := bs.Create(ctx, "ru_other", pOther, "X", "")
	sprint("ru_other", o.ID, "2026-09-10", "", [2]string{"1", "open"})

	from, to := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	f := func(p *float64) float64 {
		if p == nil {
			return -1
		}
		return *p
	}

	pr, err := s.ProjectReport(ctx, "ru_org", p1, from, to, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if len(pr.Boards) != 2 || pr.Boards[0].BoardName != "Backend" || pr.Boards[1].BoardName != "Design" {
		t.Fatalf("project boards (no archived, no kanban): %d", len(pr.Boards))
	}
	if pr.Points == nil || pr.Points.Planned != 8 || pr.Points.Done != 8 || pr.Tickets == nil || pr.Tickets.Planned != 2 || pr.Tickets.Done != 1 {
		t.Errorf("points and tickets kept apart: %+v %+v", pr.Points, pr.Tickets)
	}
	if pr.Overall.Sprints != 2 || f(pr.Overall.Predictability) != 0.75 || pr.Overall.GoalsSet != 2 || pr.Overall.GoalsMet != 1 {
		t.Errorf("overall: %+v %v", pr.Overall, f(pr.Overall.Predictability))
	}
	if len(pr.Months) != 2 || pr.Months[0].Month != "2026-08" || pr.Months[0].PointsDone != 8 || f(pr.Months[0].Predictability) != 1 ||
		pr.Months[1].Month != "2026-09" || pr.Months[1].TicketsDone != 1 || f(pr.Months[1].Predictability) != 0.5 {
		t.Errorf("months: %+v", pr.Months)
	}
	if pr.Summary != "Across 2 boards, delivered 75% of planned work in 2 sprints. 1 of 2 sprint goals met." {
		t.Errorf("summary: %q", pr.Summary)
	}

	org, err := s.OrgReport(ctx, "ru_org", from, to, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if len(org.Projects) != 2 || org.Projects[0].Name != "Shop" || org.Projects[1].Name != "Website" || org.Projects[0].Boards != nil {
		t.Fatalf("org projects: %d", len(org.Projects))
	}
	if org.Overall.Sprints != 3 || org.Points == nil || org.Points.Done != 10 || f(org.Overall.Predictability) < 0.83 || f(org.Overall.Predictability) > 0.84 {
		t.Errorf("org overall: %+v points %+v", org.Overall, org.Points)
	}
	if strings.Contains(org.Summary, "Theirs") || org.Overall.Sprints > 3 {
		t.Error("another organization's sprints counted")
	}

	// over HTTP: the org report is for admins; a project of another org isn't found
	h := NewSystem(db, organizations.Admins{DB: db})
	session := func(user string) context.Context {
		return clerk.ContextWithSessionClaims(ctx, func() *clerk.SessionClaims {
			c := &clerk.SessionClaims{RegisteredClaims: clerk.RegisteredClaims{Subject: user}}
			c.ActiveOrganizationID = "ru_org"
			return c
		}())
	}
	orgGet := func(c context.Context) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/api/org/reports/sprints?from=2026-08-01&to=2026-09-30", nil).WithContext(c)
		rec := httptest.NewRecorder()
		h.OrgReport(rec, req)
		return rec
	}
	if rec := orgGet(session("ru_admin")); rec.Code != 200 {
		t.Errorf("admin org report: %d %s", rec.Code, rec.Body)
	} else {
		var got Rollup
		json.Unmarshal(rec.Body.Bytes(), &got)
		if got.Overall.Sprints != 3 {
			t.Errorf("admin org report over http: %+v", got.Overall)
		}
	}
	if rec := orgGet(session("ru_member")); rec.Code != 403 {
		t.Errorf("member org report: %d", rec.Code)
	}
	projGet := func(id, org string) int {
		req := httptest.NewRequest("GET", "/api/projects/"+id+"/reports/sprints", nil)
		req = req.WithContext(auth.WithAPIKey(req.Context(), "u", org, "k", "ai"))
		req.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		h.ProjectReport(rec, req)
		return rec.Code
	}
	if projGet(p1, "ru_org") != 200 || projGet(pOther, "ru_org") != 404 {
		t.Error("project report access")
	}
}
