package tickets

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
)

func TestPriorityFilters(t *testing.T) {
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
	if _, err := db.Exec(ctx, `DELETE FROM projects WHERE owner_clerk_id = 'pf1'`); err != nil {
		t.Fatal(err)
	}
	var pid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('pf1', 'p') RETURNING id`).Scan(&pid)
	s := Store{DB: db}
	for title, prio := range map[string]string{"u bug": "urgent", "h bug": "high", "m task": "medium", "l task": "low"} {
		typ := "bug"
		if strings.HasSuffix(title, "task") {
			typ = "task"
		}
		tk, _ := s.CreateBacklog(ctx, "pf1", pid, typ, title, "")
		_ = s.Update(ctx, "pf1", tk.ID, "", title, "", prio)
		if title == "u bug" || title == "h bug" {
			if err := s.SetLabels(ctx, "pf1", tk.ID, []string{"needs-review"}); err != nil {
				t.Fatal(err)
			}
		}
	}

	// backlog page: list and counts follow the priority filter
	page, err := s.PageBacklog(ctx, "pf1", pid, "", nil, 1, 25, "urgent", "high")
	if err != nil || len(page.Tickets) != 2 || page.Counts["all"] != 2 || page.Counts["bug"] != 2 || page.Counts["task"] != 0 {
		t.Fatalf("backlog urgent+high: %+v %v", page, err)
	}
	if page, _ := s.PageBacklog(ctx, "pf1", pid, "task", nil, 1, 25, "low"); len(page.Tickets) != 1 || page.Tickets[0].Title != "l task" {
		t.Fatalf("type + priority: %+v", page.Tickets)
	}
	filtered, err := s.PageBacklog(ctx, "pf1", pid, "", []string{"NEEDS-REVIEW"}, 1, 1, "urgent", "high")
	if err != nil || len(filtered.Tickets) != 1 || filtered.Counts["all"] != 2 || filtered.Counts["bug"] != 2 {
		t.Fatalf("label + priority before pagination: %+v %v", filtered, err)
	}
	second, err := s.PageBacklog(ctx, "pf1", pid, "", []string{"needs-review"}, 2, 1, "urgent", "high")
	if err != nil || len(second.Tickets) != 1 || second.Counts["all"] != 2 {
		t.Fatalf("second filtered page: %+v %v", second, err)
	}
	if page, _ := s.PageBacklog(ctx, "pf1", pid, "", nil, 1, 25); len(page.Tickets) != 4 {
		t.Fatal("no priority filter = all")
	}
	if _, err := s.PageBacklog(ctx, "pf1", pid, "", nil, 1, 25, "asap"); !errors.Is(err, ErrBadPriority) {
		t.Fatalf("bad priority: %v", err)
	}
	// open tickets
	if open, _ := s.OpenTickets(ctx, "pf1", pid, OpenFilter{Priorities: []string{"urgent"}}); len(open) != 1 || open[0].Title != "u bug" {
		t.Fatalf("open urgent: %+v", open)
	}
	// HTTP: repeatable ?priority=, 400 on nonsense
	h := NewSystem(db)
	get := func(hf func(*httptest.ResponseRecorder, string), q string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		hf(rec, q)
		return rec
	}
	backlog := func(rec *httptest.ResponseRecorder, q string) {
		req := httptest.NewRequest("GET", "/?"+q, nil).WithContext(auth.WithAPIKeyUser(ctx, "pf1", "claude"))
		req.SetPathValue("id", pid)
		h.PageBacklog(rec, req)
	}
	open := func(rec *httptest.ResponseRecorder, q string) {
		req := httptest.NewRequest("GET", "/?"+q, nil).WithContext(auth.WithAPIKeyUser(ctx, "pf1", "claude"))
		req.SetPathValue("id", pid)
		h.OpenTickets(rec, req)
	}
	if rec := get(backlog, "priority=medium&priority=low"); rec.Code != 200 || strings.Contains(rec.Body.String(), "u bug") || !strings.Contains(rec.Body.String(), "m task") {
		t.Fatalf("http backlog: %d %s", rec.Code, rec.Body)
	}
	for _, hf := range []func(*httptest.ResponseRecorder, string){backlog, open} {
		if rec := get(hf, "priority=asap"); rec.Code != 400 {
			t.Errorf("bad priority: %d", rec.Code)
		}
	}
}
