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

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/boards"
	"github.com/tracklines/backend/internal/tickets"
)

// TL-397: a board's sprint report: planned at the start, added and removed along the way, done,
// carried over, predictability, velocity and the work mix; older sprints from what they recorded.
func TestBoardReport(t *testing.T) {
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
	if _, err := db.Exec(ctx, `DELETE FROM boards WHERE owner_clerk_id IN ('rp1', 'rp2'); DELETE FROM projects WHERE owner_clerk_id IN ('rp1', 'rp2')`); err != nil {
		t.Fatal(err)
	}
	var pid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('rp1', 'p') RETURNING id`).Scan(&pid)
	bs, ts, s := boards.Store{DB: db}, tickets.Store{DB: db}, Store{DB: db}
	b, _ := bs.Create(ctx, "rp1", pid, "Backend", "")
	db.Exec(ctx, `UPDATE boards SET estimate_scale = 'none' WHERE id = $1`, b.ID) // counted in tickets
	todo, done := b.Columns[0].ID, b.Columns[len(b.Columns)-1].ID

	// a sprint has an older, closed sprint from before reports were recorded: it only kept what it held at close
	db.Exec(ctx, `INSERT INTO sprints (board_id, number, length_days, starts_at, ends_at, closed_at, scope_tickets, goal)
		VALUES ($1, 1, 14, now() - interval '40 days', now() - interval '26 days', now() - interval '26 days', 4, 'Old goal')`, b.ID)
	var old string
	db.QueryRow(ctx, `SELECT id FROM sprints WHERE board_id = $1 AND number = 1`, b.ID).Scan(&old)
	oldDone, _ := ts.Create(ctx, "rp1", todo, "bug", "old fixed", "")
	_ = ts.Move(ctx, "rp1", oldDone.ID, done, 0)
	db.Exec(ctx, `UPDATE tickets SET sprint_id = $2 WHERE id = $1`, oldDone.ID, old)

	// sprint 2 starts with A (a bug), B and C; then D is added, C goes back to the backlog, A and D are finished
	a, _ := ts.Create(ctx, "rp1", todo, "bug", "A", "")
	bt, _ := ts.Create(ctx, "rp1", todo, "feature", "B", "")
	c, _ := ts.Create(ctx, "rp1", todo, "task", "C", "")
	sp2, err := s.Start(ctx, "rp1", b.ID, 14)
	if err != nil {
		t.Fatal(err)
	}
	db.Exec(ctx, `UPDATE sprints SET goal = 'Ship search' WHERE id = $1`, sp2.ID)
	d, _ := ts.Create(ctx, "rp1", todo, "feature", "D", "")
	if err := ts.ToBacklog(ctx, "rp1", c.ID); err != nil {
		t.Fatal(err)
	}
	_ = ts.Move(ctx, "rp1", a.ID, done, 0)
	_ = ts.Move(ctx, "rp1", d.ID, done, 0)
	sp3, err := s.CloseWithOptions(ctx, "rp1", sp2.ID, CloseOptions{StartNext: true}) // B carries over into sprint 3
	if err != nil || sp3 == nil {
		t.Fatalf("close: %v", err)
	}
	// sprint 3 (open): started with B; B is finished so far
	_ = ts.Move(ctx, "rp1", bt.ID, done, 0)

	now := time.Now()
	rep, err := s.BoardReport(ctx, "rp1", b.ID, now.AddDate(0, 0, -20), now.AddDate(0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Unit != "tickets" || rep.BoardName != "Backend" || len(rep.Sprints) != 2 {
		t.Fatalf("report: %+v", rep)
	}
	s2, s3 := rep.Sprints[0], rep.Sprints[1]
	f := func(p *float64) float64 {
		if p == nil {
			return -1
		}
		return *p
	}
	if s2.Number != 2 || s2.Planned != 3 || s2.Done != 2 || f(s2.Added) != 1 || f(s2.Removed) != 1 || f(s2.Carried) != 1 || s2.Estimated {
		t.Errorf("sprint 2: planned %v done %v added %v removed %v carried %v", s2.Planned, s2.Done, f(s2.Added), f(s2.Removed), f(s2.Carried))
	}
	if p := f(s2.Predictability); p < 0.66 || p > 0.67 {
		t.Errorf("sprint 2 predictability: %v", p)
	}
	if s2.GoalMet == nil || *s2.GoalMet { // B carried over: the goal wasn't met
		t.Errorf("sprint 2 goal met: %v", s2.GoalMet)
	}
	if len(s2.Mix) != 2 || s2.Mix[0].Type != "bug" || s2.Mix[0].Count != 1 || s2.Mix[1].Type != "feature" || s2.Mix[1].Count != 1 {
		t.Errorf("sprint 2 mix: %+v", s2.Mix)
	}
	if f(s2.Velocity) != 1.5 { // (sprint 1's 1 + sprint 2's 2) / 2
		t.Errorf("sprint 2 velocity: %v", f(s2.Velocity))
	}
	if s3.ClosedAt != nil || s3.Planned != 1 || s3.Done != 1 || s3.Carried != nil || s3.Velocity != nil {
		t.Errorf("open sprint 3: %+v", s3)
	}
	// totals count closed sprints only; the period before has old sprint 1, from what it recorded
	if rep.Totals.Sprints != 1 || rep.Totals.Done != 2 || f(rep.Totals.Added) != 1 || f(rep.Totals.Carried) != 1 {
		t.Errorf("totals: %+v", rep.Totals)
	}
	if rep.Previous.Sprints != 1 || rep.Previous.Planned != 4 || rep.Previous.Done != 1 || rep.Previous.Added != nil {
		t.Errorf("previous (old sprint, estimated from its close): %+v", rep.Previous)
	}
	if rep.Summary != "Delivered 67% of planned work across 1 sprint, up from 25%." {
		t.Errorf("summary: %q", rep.Summary)
	}

	// over HTTP: behind the flag; another org can't read it; nothing about people
	on := true
	h := System{store: s}.WithReports(func() bool { return on })
	get := func(org, query string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/api/boards/"+b.ID+"/reports/sprints"+query, nil)
		req = req.WithContext(auth.WithAPIKey(req.Context(), "user_1", org, "k", "ai"))
		req.SetPathValue("id", b.ID)
		rec := httptest.NewRecorder()
		h.BoardReport(rec, req)
		return rec
	}
	rec := get("rp1", "")
	if rec.Code != 200 {
		t.Fatalf("report over http: %d %s", rec.Code, rec.Body)
	}
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	for _, banned := range []string{"assign", "user_", "author", "actor"} {
		if strings.Contains(rec.Body.String(), banned) {
			t.Errorf("the report mentions people (%q)", banned)
		}
	}
	// ?format=md: the same report as a Markdown download (TL-399)
	if md := get("rp1", "?format=md"); md.Code != 200 || md.Header().Get("Content-Type") != "text/markdown; charset=utf-8" ||
		!strings.HasPrefix(md.Header().Get("Content-Disposition"), `attachment; filename="backend-sprints-`) || !strings.Contains(md.Body.String(), "# Sprint report: Backend") {
		t.Errorf("markdown: %d %v", md.Code, md.Header())
	}
	if get("rp2", "").Code != 404 {
		t.Error("another org read the report")
	}
	if get("rp1", "?from=2026-10-10&to=2026-10-01").Code != 400 || get("rp1", "?from=yesterday").Code != 400 || get("rp1", "?from=2020-01-01&to=2026-01-01").Code != 400 {
		t.Error("bad ranges accepted")
	}
	on = false
	if get("rp1", "").Code != 404 {
		t.Error("reports shown with the flag off")
	}
}

func TestSummary(t *testing.T) {
	p := func(f float64) *float64 { return &f }
	if got := summary(ReportTotals{}, ReportTotals{}, "points"); got != "No sprints finished in this period." {
		t.Error(got)
	}
	if got := summary(ReportTotals{Sprints: 3, Predictability: p(0.7)}, ReportTotals{Sprints: 2, Predictability: p(0.9)}, "points"); got != "Delivered 70% of planned work across 3 sprints, down from 90%." {
		t.Error(got)
	}
	if got := summary(ReportTotals{Sprints: 2, Predictability: p(0.8)}, ReportTotals{Sprints: 1, Predictability: p(0.8)}, "points"); got != "Delivered 80% of planned work across 2 sprints, the same as before." {
		t.Error(got)
	}
	if got := summary(ReportTotals{Sprints: 2, Done: 13}, ReportTotals{}, "points"); got != "Delivered 13 points across 2 sprints." {
		t.Error(got)
	}
}
