package tickets

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/httpx"
	"github.com/tracklines/backend/internal/notifications"
)

// told records notifications instead of sending them.
type told struct {
	mu  sync.Mutex
	got []string // "to|text"
}

func (n *told) Notify(_ context.Context, _ string, to []string, e notifications.Event) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, p := range to {
		n.got = append(n.got, p+"|"+e.Text)
	}
	return nil
}

// Due dates (TL-631): set, cleared and validated over HTTP; filtered (overdue/soon/none, in the
// caller's day) and sorted in search and the backlog; reminded once, the day before and on the day.
func TestDueDates(t *testing.T) {
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
	const org = "org_due"
	var pid string
	if err := db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ($1, 'Due') RETURNING id`, org).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	s := Store{DB: db}
	mk := func(title string) string {
		t.Helper()
		tk, err := s.CreateBacklog(ctx, org, pid, "task", title, "")
		if err != nil {
			t.Fatal(err)
		}
		return tk.ID
	}

	// over HTTP: set, read back, bad date 400, omitted unchanged, "" clears
	a := mk("ship it")
	h := NewSystem(db)
	c := auth.WithAPIKey(ctx, "user_d", org, "ui", "")
	update := func(id, body string) int {
		req := httptest.NewRequest("PATCH", "/", strings.NewReader(body)).WithContext(c)
		req.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		h.Update(rec, req)
		return rec.Code
	}
	if code := update(a, `{"title":"ship it","due_date":"2026-13-01"}`); code != 400 {
		t.Fatalf("bad date → %d, want 400", code)
	}
	if code := update(a, `{"title":"ship it","due_date":"2026-11-02"}`); code != 204 {
		t.Fatalf("set → %d", code)
	}
	if d, _ := s.Get(ctx, org, a); d.DueDate == nil || *d.DueDate != "2026-11-02" {
		t.Fatalf("due = %v", d.DueDate)
	}
	if update(a, `{"title":"renamed"}`); func() bool { d, _ := s.Get(ctx, org, a); return d.DueDate == nil }() {
		t.Fatal("omitting due_date must leave it")
	}
	if code := update(a, `{"title":"renamed","due_date":""}`); code != 204 {
		t.Fatalf("clear → %d", code)
	}
	if d, _ := s.Get(ctx, org, a); d.DueDate != nil {
		t.Fatal(`"" should clear it`)
	}
	if err := s.SetDueDate(ctx, "org_other", a, "2026-11-02"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another org: %v", err)
	}

	// search: overdue / soon / none by the caller's day; done tickets are never overdue
	today := "2026-10-25"
	set := func(id, day string) {
		t.Helper()
		if err := s.SetDueDate(ctx, org, id, day); err != nil {
			t.Fatal(err)
		}
	}
	late, todayT, tomorrow, later, doneLate := mk("late"), mk("today"), mk("tomorrow"), mk("later"), mk("done late")
	mk("none") // no due date
	set(late, "2026-10-20")
	set(todayT, today)
	set(tomorrow, "2026-10-26")
	set(later, "2026-11-30")
	set(doneLate, "2026-10-01")
	if _, err := db.Exec(ctx, `UPDATE tickets SET resolved_at = now(), resolution = 'done' WHERE id = $1`, doneLate); err != nil {
		t.Fatal(err)
	}
	titles := func(q SearchQuery) []string {
		t.Helper()
		q.ProjectID, q.Today, q.Limit = pid, today, 50
		rs, err := s.Search(ctx, org, q)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range rs {
			out = append(out, r.Title)
		}
		slices.Sort(out)
		return out
	}
	if got := titles(SearchQuery{Due: "overdue"}); !slices.Equal(got, []string{"late"}) {
		t.Fatalf("overdue: %v", got)
	}
	if got := titles(SearchQuery{Due: "soon"}); !slices.Equal(got, []string{"today", "tomorrow"}) {
		t.Fatalf("soon: %v", got)
	}
	if got := titles(SearchQuery{Due: "none"}); !slices.Contains(got, "none") || slices.Contains(got, "late") {
		t.Fatalf("none: %v", got)
	}
	rs, _ := s.Search(ctx, org, SearchQuery{ProjectID: pid, SortDue: true, Limit: 50})
	var order []string
	for _, r := range rs {
		order = append(order, r.Title)
	}
	if len(order) < 6 || !slices.Equal(order[:5], []string{"done late", "late", "today", "tomorrow", "later"}) {
		t.Fatalf("sort=due: %v", order)
	}

	// over HTTP the day is the caller's: in Auckland it's already tomorrow, so "today" is overdue there
	search := func(query string, loc *time.Location) (int, string) {
		req := httptest.NewRequest("GET", "/api/search?project="+pid+"&"+query, nil).WithContext(httpx.WithZoneContext(c, loc))
		rec := httptest.NewRecorder()
		h.Search(rec, req)
		return rec.Code, rec.Body.String()
	}
	if code, _ := search("due=later", time.UTC); code != 400 {
		t.Fatalf("bad due → %d", code)
	}
	if code, _ := search("sort=title", time.UTC); code != 400 {
		t.Fatalf("bad sort → %d", code)
	}
	nz, _ := time.LoadLocation("Pacific/Auckland")
	now := time.Now()
	set(todayT, now.In(time.UTC).Format(time.DateOnly))
	if now.In(nz).Format(time.DateOnly) != now.UTC().Format(time.DateOnly) {
		if _, body := search("due=overdue", nz); !strings.Contains(body, `"title":"today"`) {
			t.Fatalf("Auckland's overdue should hold UTC's today: %s", body)
		}
	}

	// backlog sort=due: soonest first, undated last
	page, err := s.PageBacklogSorted(ctx, org, pid, "", nil, 1, 50, "due")
	if err != nil {
		t.Fatal(err)
	}
	if page.Tickets[0].Title != "late" || page.Tickets[len(page.Tickets)-1].DueDate != nil {
		t.Fatalf("backlog sort=due: first %q, last due %v", page.Tickets[0].Title, page.Tickets[len(page.Tickets)-1].DueDate)
	}

	// reminders: the assignee and watchers, once, today and tomorrow, open tickets only
	day := time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)
	set(todayT, "2026-10-25")
	if _, err := db.Exec(ctx, `UPDATE tickets SET assigned_to = 'user_amy' WHERE id = ANY($1)`, []string{todayT, tomorrow}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO ticket_mentions (org_id, user_id, ticket_id) VALUES ($1, 'user_bo', $2)`, org, todayT); err != nil {
		t.Fatal(err)
	}
	n := &told{}
	r := DueReminders{Store: s, Notify: n}
	if _, err := r.Send(ctx, day); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(n.got, "\n")
	for _, want := range []string{"user_amy|", "is due today", "user_bo|", "is due tomorrow"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("reminders lack %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "done late") || strings.Contains(joined, "later") {
		t.Fatalf("reminded the wrong tickets:\n%s", joined)
	}
	before := len(n.got)
	if sent, err := r.Send(ctx, day); err != nil || sent != 0 || len(n.got) != before {
		t.Fatalf("second run sent %d (%v): reminders must go once", sent, err)
	}
}
