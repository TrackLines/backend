package tickets

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"slices"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
)

// TL-846: Sort by priority puts urgent at the top and low at the bottom, ties in the list's usual order,
// for the backlog and search (My work is search). GET /api/priorities lists the levels in that order.
func TestSortByPriority(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	ctx := context.Background()
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
	if _, err := db.Exec(ctx, `DELETE FROM boards WHERE owner_clerk_id = 'ps1'; DELETE FROM projects WHERE owner_clerk_id = 'ps1'`); err != nil {
		t.Fatal(err)
	}

	// the Go list is the enum's order, most urgent first
	var enum []string
	if err := db.QueryRow(ctx, `SELECT array_agg(e::text ORDER BY e DESC) FROM unnest(enum_range(NULL::priority)) e`).Scan(&enum); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(enum, Priorities) {
		t.Fatalf("Priorities %v don't match the priority enum %v", Priorities, enum)
	}

	var pid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('ps1', 'p') RETURNING id`).Scan(&pid)
	s := Store{DB: db}
	// backlog order: low-a, urgent-a, medium, low-b, high, urgent-b
	for _, tk := range []struct{ title, priority string }{
		{"low-a", "low"}, {"urgent-a", "urgent"}, {"medium", "medium"}, {"low-b", "low"}, {"high", "high"}, {"urgent-b", "urgent"},
	} {
		c, err := s.CreateBacklog(ctx, "ps1", pid, "task", tk.title, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Update(ctx, "ps1", c.ID, "", tk.title, "", tk.priority); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"urgent-a", "urgent-b", "high", "medium", "low-a", "low-b"} // urgent top, low bottom, ties in backlog order

	page, err := s.PageBacklogSorted(ctx, "ps1", pid, "", nil, 1, 10, "priority")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tk := range page.Tickets {
		got = append(got, tk.Title)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("backlog sort=priority: %v", got)
	}

	// over HTTP, and the same order for search (My work is search with assignee=me)
	h := NewSystem(db)
	as := auth.WithAPIKeyUser(ctx, "ps1", "claude")
	req := httptest.NewRequest("GET", "/?sort=priority", nil).WithContext(as)
	req.SetPathValue("id", pid)
	rec := httptest.NewRecorder()
	h.PageBacklog(rec, req)
	var bp BacklogPage
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &bp) != nil || len(bp.Tickets) != 6 || bp.Tickets[0].Priority != "urgent" || bp.Tickets[5].Priority != "low" {
		t.Fatalf("http backlog: %d %s", rec.Code, rec.Body)
	}

	results, err := s.Search(ctx, "ps1", SearchQuery{ProjectID: pid, Limit: 10, SortPriority: true})
	if err != nil {
		t.Fatal(err)
	}
	var levels []string
	for _, r := range results {
		levels = append(levels, r.Priority)
	}
	if !slices.Equal(levels, []string{"urgent", "urgent", "high", "medium", "low", "low"}) {
		t.Fatalf("search sort=priority: %v", levels)
	}
	for q, code := range map[string]int{"sort=priority": 200, "sort=due": 200, "sort=random": 400} {
		req := httptest.NewRequest("GET", "/?project="+pid+"&"+q, nil).WithContext(as)
		rec := httptest.NewRecorder()
		h.Search(rec, req)
		if rec.Code != code {
			t.Fatalf("search %s: %d %s", q, rec.Code, rec.Body)
		}
	}

	// the levels, most urgent first, for clients' pickers
	rec = httptest.NewRecorder()
	ListPriorities(rec, httptest.NewRequest("GET", "/", nil))
	var list []struct{ Key, Name string }
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &list) != nil || len(list) != 4 ||
		list[0].Key != "urgent" || list[0].Name != "Urgent" || list[3].Key != "low" {
		t.Fatalf("priorities: %d %s", rec.Code, rec.Body)
	}
}
