package roadmaps

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/tracklines/backend/internal/workweek"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/httpx"
	"github.com/tracklines/backend/internal/organizations"
)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func atp(s string) *time.Time { t := at(s); return &t }
func strp(s string) *string   { return &s }

// TL-616: weekly from the earliest ticket's Monday to this week, counting what existed and was done
// by each week's end.
func TestBurnup(t *testing.T) {
	now := at("2026-10-22T12:00:00Z") // a Thursday
	stamps := []stamp{
		{created: at("2026-10-06T09:00:00Z"), done: atp("2026-10-13T09:00:00Z")}, // done in the week of 12 Oct
		{created: at("2026-10-08T09:00:00Z")},
		{created: at("2026-10-19T00:00:00Z"), done: atp("2026-10-21T00:00:00Z")}, // this week
	}
	want := []Week{{"2026-10-05", 0, 2}, {"2026-10-12", 1, 2}, {"2026-10-19", 2, 3}}
	if got := burnup(stamps, now, time.UTC, workweek.Default()); !reflect.DeepEqual(got, want) {
		t.Errorf("burnup:\n got %+v\nwant %+v", got, want)
	}
	// weeks are the caller's: 23:30 UTC on Sunday 18th is Monday 19th in Auckland
	nz, _ := time.LoadLocation("Pacific/Auckland")
	got := burnup([]stamp{{created: at("2026-10-18T23:30:00Z")}}, now, nz, workweek.Default())
	if len(got) != 1 || got[0].WeekStart != "2026-10-19" || got[0].Total != 1 {
		t.Errorf("in Auckland: %+v", got)
	}
	// an old ticket doesn't stretch it past 12 weeks back
	got = burnup([]stamp{{created: at("2025-01-01T00:00:00Z")}}, now, time.UTC, workweek.Default())
	if len(got) != burnWeeks+1 || got[0].WeekStart != "2026-07-27" || got[0].Total != 1 {
		t.Errorf("capped: %d weeks from %+v", len(got), got[0])
	}
	if got := burnup(nil, now, time.UTC, workweek.Default()); got == nil || len(got) != 0 {
		t.Errorf("no tickets: %#v", got)
	}
}

func TestMermaidLabel(t *testing.T) {
	for in, want := range map[string]string{
		`Search "v2": fast; #1`:  "Search 'v2' - fast, 1",
		"Line\none\r\ntwo  `x`":  "Line one two 'x'",
		"<script>alert</script>": "scriptalert/script",
		"  plain  ":              "plain",
	} {
		if got := mermaidLabel(in); got != want {
			t.Errorf("mermaidLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

var reportNow = time.Date(2026, 10, 22, 9, 30, 0, 0, time.UTC)

func reportRoadmap() *Roadmap {
	return &Roadmap{
		Title: "Q4 | <b>plan</b>", Description: "Where we're going.", Progress: Progress{Done: 3, Total: 5},
		Items: []Item{
			{Title: "Search: \"fast\"; #1", StartDate: strp("2026-09-01"), TargetDate: strp("2026-09-30"), Status: StatusDone, Progress: Progress{Done: 2, Total: 2},
				Tickets: []LinkedTicket{{Key: "TL-1", Title: "Index | rebuild", Done: true}, {Key: "TL-2", Title: "Ranking\nv2", Done: true}}},
			{Title: "Billing", StartDate: strp("2026-09-15"), TargetDate: strp("2026-10-10"), Status: StatusInProgress, Progress: Progress{Done: 1, Total: 3},
				Tickets: []LinkedTicket{{Key: "TL-3", Title: "Invoices", Done: true}, {Title: "No key"}}},
			{Title: "Mobile", TargetDate: strp("2026-12-01"), Status: StatusInProgress},
			{Title: "Someday", Status: StatusNotStarted},
		},
	}
}

// TL-616: the report as Markdown: every section and chart, each chart with its table.
func TestRoadmapMarkdown(t *testing.T) {
	updates := []Update{
		{Health: HealthOnTrack, Note: "Started", PublishedAt: "2026-09-01T10:00:00.000000Z", Author: "Ava"},
		{Health: HealthAtRisk, Note: "Billing | slipping\nagain", PublishedAt: "2026-10-20T10:00:00.000000Z", Author: "Lee"},
	}
	weeks := []Week{{"2026-10-12", 2, 4}, {"2026-10-19", 3, 5}}
	md := Markdown(reportRoadmap(), updates, weeks, reportNow, time.UTC)
	order := []string{
		"# Roadmap report: Q4 \\| &lt;b&gt;plan&lt;/b&gt;", "Generated 22 Oct 2026 09:30", "Where we're going.",
		"**Overall progress: 3 of 5 (60%) tickets done**",
		"## Latest update", "| 20 Oct 2026 | At risk | Billing \\| slipping again | Lee |",
		"## Items", "### 1. Search: \"fast\"; #1", "2026-09-01 → 2026-09-30 · Done · progress 2 of 2 (100%)",
		"| TL-1 | Index \\| rebuild | Yes |", "| TL-2 | Ranking v2 | Yes |",
		"### 2. Billing", "2026-09-15 → 2026-10-10 · Late · progress 1 of 3 (33%)", "| – | No key | No |",
		"### 3. Mobile", "– → 2026-12-01 · In progress · progress –",
		"### 4. Someday", "– → – · Not started",
		"## Items by status", "pie title Items by status", `"Done" : 1`, `"In progress" : 1`, `"Late" : 1`, "| Not started | 1 |",
		"## Timeline", "gantt", "dateFormat YYYY-MM-DD",
		"Search - 'fast', 1 :done, i1, 2026-09-01, 2026-09-30",
		"Billing :crit, i2, 2026-09-15, 2026-10-10",
		"Mobile :active, milestone, i3, 2026-12-01, 0d",
		"| Someday | – | – | Not started |",
		"## Tickets per item", "xychart-beta", `x-axis ["Search - 'fast', 1", "Billing", "Mobile", "Someday"]`, "bar [2, 3, 0, 0]", "bar [2, 1, 0, 0]", "| Billing | 1 | 3 |",
		"## Burn-up", `x-axis ["2026-10-12", "2026-10-19"]`, "line [2, 3]", "line [4, 5]", "| 2026-10-19 | 3 | 5 |",
		"## Health over time", "| 1 Sep 2026 | On track | Started | Ava |", "| 20 Oct 2026 | At risk |",
	}
	at := 0
	for _, s := range order {
		i := strings.Index(md[at:], s)
		if i < 0 {
			t.Fatalf("missing or out of order: %q\n%s", s, md)
		}
		at += i
	}
	if strings.Contains(md, "Someday :") {
		t.Errorf("undated items stay out of the gantt:\n%s", md)
	}
	// nothing but plain text in a cell: no raw pipes, newlines or HTML from titles and notes
	if strings.Contains(md, "<b>") || strings.Contains(md, "Ranking\nv2") {
		t.Errorf("unescaped:\n%s", md)
	}

	// a bare roadmap: no empty sections or charts
	md = Markdown(&Roadmap{Title: "Empty"}, nil, []Week{}, reportNow, time.UTC)
	for _, s := range []string{"## Items", "## Latest update", "## Burn-up", "## Health", "```mermaid"} {
		if strings.Contains(md, s) {
			t.Errorf("empty roadmap has %q:\n%s", s, md)
		}
	}
	if !strings.Contains(md, "**Overall progress: – tickets done**") {
		t.Errorf("empty roadmap:\n%s", md)
	}
}

// TL-616: the endpoint: for the roadmap's managers only, burn-up counting an epic's sub-tickets.
func TestReportEndpoint(t *testing.T) {
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

	const org = "rrp-org"
	for _, q := range []string{
		`DELETE FROM roadmaps WHERE owner_clerk_id IN ('rrp-org', 'rrp-other')`,
		`DELETE FROM boards WHERE owner_clerk_id = 'rrp-org'`,
		`DELETE FROM projects WHERE owner_clerk_id IN ('rrp-org', 'rrp-other')`,
		`DELETE FROM teams WHERE org_id = 'rrp-org'`,
		`DELETE FROM org_admins WHERE org_id IN ('rrp-org', 'rrp-other')`,
		`INSERT INTO org_admins (org_id, user_clerk_id, granted_by) VALUES ('rrp-org', 'rrp_admin', 'test'), ('rrp-other', 'rrp_x', 'test')`,
	} {
		if _, err := db.Exec(ctx, q); err != nil {
			t.Fatal(q, err)
		}
	}
	var p, team string
	db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ($1, 'P') RETURNING id`, org).Scan(&p)
	db.QueryRow(ctx, `INSERT INTO teams (org_id, name) VALUES ($1, 'T') RETURNING id`, org).Scan(&team)
	for _, q := range []string{
		`INSERT INTO team_members (team_id, user_clerk_id, leader) VALUES ('` + team + `', 'rrp_lead', true), ('` + team + `', 'rrp_mem', false)`,
		`INSERT INTO boards (project_id, owner_clerk_id, name, team_id) VALUES ('` + p + `', 'rrp-org', 'Backend', '` + team + `')`,
	} {
		if _, err := db.Exec(ctx, q); err != nil {
			t.Fatal(q, err)
		}
	}
	// tickets made daysAgo, done (resolved) doneAgo days ago when doneAgo >= 0
	ticket := func(typ, title string, parent *string, daysAgo, doneAgo int) string {
		var id string
		if err := db.QueryRow(ctx, `INSERT INTO tickets (project_id, type, title, position, created_by, parent_id, created_at, resolved_at, done_at)
			VALUES ($1, $2::ticket_type, $3, 0, 'u', $4, now() - make_interval(days => $5),
				CASE WHEN $6 >= 0 THEN now() - make_interval(days => $6) END, CASE WHEN $6 >= 0 THEN now() - make_interval(days => $6) END) RETURNING id`,
			p, typ, title, parent, daysAgo, doneAgo).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	epic := ticket("epic", "Checkout", nil, 30, -1)
	ticket("task", "pay", &epic, 30, 20)
	ticket("task", "ship", &epic, 10, -1)
	direct := ticket("feature", "Search", nil, 3, 1)

	s := Store{DB: db}
	rm, err := s.Create(ctx, org, p, "Launch plan", "", Team)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceItems(ctx, rm.ID, org, []Item{{Title: "Checkout", StartDate: strp("2026-01-01"), TargetDate: strp("2026-02-01")}, {Title: "Search"}}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(ctx, rm.ID)
	if err := s.SetItemTickets(ctx, org, rm.ID, got.Items[0].ID, []string{epic}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetItemTickets(ctx, org, rm.ID, got.Items[1].ID, []string{direct}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateUpdate(ctx, rm.ID, org, HealthAtRisk, "Checkout slipping", "rrp_admin", "Ava"); err != nil {
		t.Fatal(err)
	}

	h := NewSystem(db).WithManagers(organizations.Admins{DB: db})
	get := func(user, inOrg, query string) *httptest.ResponseRecorder {
		c := httpx.WithZoneContext(auth.WithAPIKey(ctx, user, inOrg, user+"-key", "ai"), time.UTC)
		req := httptest.NewRequest(http.MethodGet, "/"+query, nil).WithContext(c)
		req.SetPathValue("id", rm.ID)
		rec := httptest.NewRecorder()
		h.Report(rec, req)
		return rec
	}
	for who, want := range map[[2]string]int{
		{"rrp_admin", org}: http.StatusOK, {"rrp_lead", org}: http.StatusOK,
		{"rrp_mem", org}: http.StatusForbidden, {"rrp_x", "rrp-other"}: http.StatusNotFound,
	} {
		rec := get(who[0], who[1], "")
		if rec.Code != want {
			t.Errorf("%s: %d, want %d", who[0], rec.Code, want)
		}
		if want == http.StatusForbidden && !strings.Contains(rec.Body.String(), "team leaders") {
			t.Errorf("%s: %s", who[0], rec.Body)
		}
	}

	var out struct{ Burnup []Week }
	rec := get("rrp_lead", org, "")
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(rec.Body, err)
	}
	b := out.Burnup
	// from the week the first sub-ticket was made to this week; the epic itself never counts
	if first := weekOf(time.Now().AddDate(0, 0, -30), time.UTC, workweek.Default()).Format(time.DateOnly); len(b) < 5 || b[0].WeekStart != first || b[0].Total != 1 || b[0].Done != 0 {
		t.Fatalf("burn-up start (want %s): %+v", first, b)
	}
	if last := b[len(b)-1]; last.WeekStart != weekOf(time.Now(), time.UTC, workweek.Default()).Format(time.DateOnly) || last.Total != 3 || last.Done != 2 {
		t.Errorf("this week: %+v", last)
	}

	rec = get("rrp_admin", org, "?format=md")
	md := rec.Body.String()
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/markdown; charset=utf-8" || rec.Header().Get("X-Content-Type-Options") != "nosniff" ||
		rec.Header().Get("Content-Disposition") != `attachment; filename="launch-plan-roadmap-`+time.Now().UTC().Format(time.DateOnly)+`.md"` {
		t.Fatalf("markdown: %d %v", rec.Code, rec.Header())
	}
	for _, s := range []string{"# Roadmap report: Launch plan", "**Overall progress: 2 of 3 (67%) tickets done**", "| At risk | Checkout slipping | Ava |",
		"| Checkout | 1 | 2 |", "| Search | 1 | 1 |", "Checkout :crit, i1, 2026-01-01, 2026-02-01", "## Burn-up", "-" + "1 | Checkout | No |"} {
		if !strings.Contains(md, s) {
			t.Errorf("markdown missing %q:\n%s", s, md)
		}
	}
	if code := get("rrp_mem", org, "?format=md").Code; code != http.StatusForbidden {
		t.Errorf("member markdown: %d", code)
	}
}
