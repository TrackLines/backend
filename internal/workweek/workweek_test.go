package workweek

import (
	"context"
	"errors"
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
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tracklines/backend/internal/auth"
)

// the default is what was hard-coded: Mon–Fri, Monday first, 09:00–17:00, 8h a day
func TestWeek(t *testing.T) {
	d := Default()
	at := func(s string) time.Time { v, _ := time.Parse(time.RFC3339, s); return v }
	if d.DayHours() != 8 || !d.Works(time.Monday) || d.Works(time.Saturday) || !d.Valid() {
		t.Fatalf("default: %+v", d)
	}
	// Fri 16:00 → Mon 10:00: an hour on Friday, an hour on Monday
	if got := d.BusinessMinutes(at("2026-10-23T16:00:00Z"), at("2026-10-26T10:00:00Z")); got != 120 {
		t.Fatalf("over a weekend: %v", got)
	}
	// a Sun–Thu week, 08:00–16:00, Sunday first: Friday counts nothing, Sunday does
	gulf := Week{Days: []int{0, 1, 2, 3, 4}, WeekStart: 0, From: 8 * 60, To: 16 * 60}
	if got := gulf.BusinessMinutes(at("2026-10-23T00:00:00Z"), at("2026-10-25T23:59:00Z")); got != 8*60 {
		t.Fatalf("Fri–Sun on a Sun–Thu week: %v", got)
	}
	if got := gulf.StartOfWeek(at("2026-10-28T12:00:00Z")); got.Format(time.DateOnly) != "2026-10-25" { // a Wednesday → that Sunday
		t.Fatalf("week start: %v", got)
	}
	if got := d.StartOfWeek(at("2026-10-25T12:00:00Z")); got.Format(time.DateOnly) != "2026-10-19" { // a Sunday → the Monday before
		t.Fatalf("Monday week: %v", got)
	}
	for _, bad := range []Week{
		{Days: nil, WeekStart: 1, From: 540, To: 1020},
		{Days: []int{1, 1}, WeekStart: 1, From: 540, To: 1020},
		{Days: []int{7}, WeekStart: 1, From: 540, To: 1020},
		{Days: []int{1}, WeekStart: 9, From: 540, To: 1020},
		{Days: []int{1}, WeekStart: 1, From: 600, To: 630},
		{Days: []int{1}, WeekStart: 1, From: 540, To: 1500},
	} {
		if bad.Valid() {
			t.Errorf("valid: %+v", bad)
		}
	}
}

func testDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	m, err := migrate.New("file://../migrations", "pgx5"+url[len("postgres"):])
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatal(err)
	}
	db, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	return db
}

// TL-702: the org's week, a project's own, back to the org's; who can set which; sprints end on a working
// day; the change is in the org log
func TestWorkingWeekSettings(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	org := "org_ww_" + uuid.NewString()[:8]
	var project, other, board string
	must := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(ctx, q, args...); err != nil {
			t.Fatal(q, err)
		}
	}
	must(`INSERT INTO org_billing (org_id, stripe_status, plan) VALUES ($1, 'active', 'pro')`, org)
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ($1, 'Support') RETURNING id`, org).Scan(&project)
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ($1, 'Web') RETURNING id`, org).Scan(&other)
	_ = db.QueryRow(ctx, `INSERT INTO boards (project_id, owner_clerk_id, name) VALUES ($1, $2, 'Desk') RETURNING id`, project, org).Scan(&board)
	s := Store{DB: db}

	// nothing set: the default, everywhere
	if w, _ := s.ForProject(ctx, project); w.DayHours() != 8 || len(w.Days) != 5 {
		t.Fatalf("default: %+v", w)
	}
	h := System{Store: s,
		IsAdmin: func(_ context.Context, _, user string) (bool, error) { return user == "user_admin", nil },
		IsProjectManager: func(_ context.Context, _, user, _ string) (bool, error) {
			return user == "user_admin" || user == "user_lead", nil
		},
	}
	session := func(user string) context.Context {
		return clerk.ContextWithSessionClaims(ctx, &clerk.SessionClaims{RegisteredClaims: clerk.RegisteredClaims{Subject: user}, Claims: clerk.Claims{ActiveOrganizationID: org}})
	}
	call := func(c context.Context, method, path, body string, handler func(w *httptest.ResponseRecorder, r *http.Request)) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(c)
		req.Header.Set("Accept", "application/json")
		req.SetPathValue("id", project)
		handler(rec, req)
		return rec
	}
	mon2thu := `{"days":[1,2,3,4],"week_start":1,"start_minute":480,"end_minute":1080}`
	expect := func(want int, rec *httptest.ResponseRecorder) {
		t.Helper()
		if rec.Code != want {
			t.Fatalf("%d %s, want %d", rec.Code, rec.Body, want)
		}
	}
	setOrg := func(w *httptest.ResponseRecorder, r *http.Request) { h.SetOrg(w, r) }
	setProject := func(w *httptest.ResponseRecorder, r *http.Request) { h.SetProject(w, r) }
	reset := func(w *httptest.ResponseRecorder, r *http.Request) { h.ResetProject(w, r) }
	expect(403, call(session("user_lead"), "PUT", "/", mon2thu, setOrg))                                    // a lead isn't an admin
	expect(403, call(auth.WithAPIKey(ctx, "user_admin", org, "", "personal"), "PUT", "/", mon2thu, setOrg)) // never a key
	expect(400, call(session("user_admin"), "PUT", "/", `{"days":[],"week_start":1,"start_minute":0,"end_minute":600}`, setOrg))
	expect(204, call(session("user_admin"), "PUT", "/", mon2thu, setOrg))
	if w, _ := s.ForProject(ctx, other); w.DayHours() != 10 || w.Works(time.Friday) {
		t.Fatalf("the org's week, for a project without its own: %+v", w)
	}

	// a support project works weekends too: its own week, set by its lead; a member can't
	everyDay := `{"days":[0,1,2,3,4,5,6],"week_start":0,"start_minute":540,"end_minute":1020}`
	expect(403, call(session("user_member"), "PUT", "/", everyDay, setProject))
	expect(204, call(session("user_lead"), "PUT", "/", everyDay, setProject))
	if w, _ := s.ForProject(ctx, project); !w.Works(time.Saturday) || w.WeekStart != 0 {
		t.Fatalf("project's own: %+v", w)
	}
	if w, _ := s.ForBoard(ctx, board); !w.Works(time.Sunday) {
		t.Fatalf("its board: %+v", w)
	}
	rec := httptest.NewRecorder()
	h.Get(rec, httptest.NewRequest("GET", "/", nil).WithContext(session("user_member")))
	if !strings.Contains(rec.Body.String(), `"org":{"days":[1,2,3,4]`) || !strings.Contains(rec.Body.String(), project) || strings.Contains(rec.Body.String(), other) {
		t.Fatalf("everyone reads it: %s", rec.Body)
	}

	// sprint ends: on its own (every-day) week a 5-day sprint ends 5 days on; back on the org's Mon–Thu,
	// one landing on a Friday runs on to Monday
	end := func(from string, days int) string {
		var got time.Time
		if err := db.QueryRow(ctx, `SELECT working_end($1::uuid, $2::timestamptz + make_interval(days => $3))`, board, from, days).Scan(&got); err != nil {
			t.Fatal(err)
		}
		return got.UTC().Format(time.DateOnly)
	}
	if got := end("2026-10-19T10:00:00Z", 5); got != "2026-10-24" {
		t.Fatalf("every day: ends %s", got)
	}
	expect(204, call(session("user_lead"), "DELETE", "/", "", reset))
	if got := end("2026-10-19T10:00:00Z", 4); got != "2026-10-26" { // Mon + 4 = Fri, off: on to Monday
		t.Fatalf("Mon–Thu: ends %s", got)
	}
	if got := end("2026-10-19T10:00:00Z", 3); got != "2026-10-22" { // Thursday: a working day
		t.Fatalf("Thursday: ends %s", got)
	}

	// the org log has who changed what
	var n int
	_ = db.QueryRow(ctx, `SELECT count(*) FROM audit_outbox WHERE org_id = $1 AND action = 'working_week.changed'`, org).Scan(&n)
	if n != 3 { // the org's, the project's own, and back to the org's
		t.Fatalf("%d working_week.changed events", n)
	}
}
