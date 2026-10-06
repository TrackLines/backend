package sprints

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/boards"
	"github.com/tracklines/backend/internal/tickets"
)

// Needs a throwaway db: TEST_DATABASE_URL=postgres://... go test ./internal/sprints/
func TestSprints(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
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
	if _, err := db.Exec(ctx, `DELETE FROM users WHERE clerk_id IN ('s1', 's2');
		INSERT INTO users (clerk_id, email) VALUES ('s1', 'a@b.c'), ('s2', 'd@e.f')`); err != nil {
		t.Fatal(err)
	}
	var pid string
	if err := db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('s1', 'p') RETURNING id`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	bs, ts, s := boards.Store{DB: db}, tickets.Store{DB: db}, Store{DB: db}
	b, err := bs.Create(ctx, "s1", pid, "Backend", "")
	if err != nil {
		t.Fatal(err)
	}
	todo, doing, done := b.Columns[0].ID, b.Columns[1].ID, b.Columns[2].ID
	add := func(col, title string) string {
		tk, err := ts.Create(ctx, "s1", col, "task", title, "")
		if err != nil {
			t.Fatal(err)
		}
		return tk.ID
	}
	view := func() (*boards.Board, map[string][]string) {
		full, err := bs.Get(ctx, b.ID, "s1")
		if err != nil {
			t.Fatal(err)
		}
		out := map[string][]string{}
		for _, c := range full.Columns {
			for _, tk := range c.Tickets {
				out[c.Name] = append(out[c.Name], tk.Title)
			}
		}
		return full, out
	}

	add(todo, "pre-sprint ticket") // board used before sprints
	if _, err := s.Start(ctx, "s1", b.ID, 0); !errors.Is(err, ErrInvalidLength) {
		t.Fatalf("length 0: %v", err)
	}
	if _, err := s.Start(ctx, "s2", b.ID, 7); !errors.Is(err, ErrNotFound) {
		t.Fatalf("non-owner start: %v", err)
	}
	s1, err := s.Start(ctx, "s1", b.ID, 14)
	if err != nil || s1.Number != 1 || s1.LengthDays != 14 {
		t.Fatalf("start: %+v %v", s1, err)
	}
	if _, err := s.Start(ctx, "s1", b.ID, 7); !errors.Is(err, ErrAlreadyOpen) {
		t.Fatalf("second open sprint: %v", err)
	}
	if full, cols := view(); full.Sprint == nil || full.Sprint.ID != s1.ID || len(cols["To do"]) != 1 {
		t.Fatalf("pre-sprint ticket not pulled into sprint 1: %+v", cols)
	}

	add(doing, "half done")
	add(done, "shipped")
	add(todo, "not started")

	s2, err := s.Close(ctx, "s1", s1.ID)
	if err != nil || s2.Number != 2 || s2.LengthDays != 14 {
		t.Fatalf("close → next: %+v %v", s2, err)
	}
	full, cols := view()
	if full.Sprint.ID != s2.ID {
		t.Fatalf("board not on sprint 2: %+v", full.Sprint)
	}
	if len(cols["To do"]) != 2 || len(cols["In progress"]) != 1 || len(cols["Done"]) != 0 {
		t.Fatalf("carry-over (unfinished keep column, done stays behind): %+v", cols)
	}
	var shippedSprint string
	_ = db.QueryRow(ctx, `SELECT sprint_id FROM tickets WHERE title = 'shipped' AND project_id = $1`, pid).Scan(&shippedSprint)
	if shippedSprint != s1.ID {
		t.Fatalf("done ticket should stay on the closed sprint")
	}
	if _, err := s.Close(ctx, "s1", s1.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("closing a closed sprint: %v", err)
	}

	// a new Done ticket in sprint 2 must count positions from 0 even though sprint 1's
	// "shipped" still sits (hidden) in the same column
	add(done, "shipped 2")
	if _, cols := view(); len(cols["Done"]) != 1 {
		t.Fatalf("done column: %+v", cols)
	}
	if full, _ := view(); full.Columns[2].Tickets[0].Position != 0 {
		t.Fatalf("hidden tickets leaked into positions: %+v", full.Columns[2].Tickets)
	}

	// auto-close: make sprint 2 overdue
	if _, err := db.Exec(ctx, `UPDATE sprints SET ends_at = now() - interval '1 minute' WHERE id = $1`, s2.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := s.AutoClose(ctx); err != nil || n < 1 {
		t.Fatalf("auto-close: %d %v", n, err)
	}
	full, cols = view()
	if full.Sprint.Number != 3 || len(cols["To do"]) != 2 || len(cols["In progress"]) != 1 {
		t.Fatalf("after auto-close: sprint %d %+v", full.Sprint.Number, cols)
	}
	if n, _ := s.AutoClose(ctx); n != 0 {
		t.Fatalf("auto-close should be idle, closed %d", n)
	}
	if list, _ := s.List(ctx, "s1", b.ID); len(list) != 3 || list[0].Number != 3 || list[2].ClosedAt == nil {
		t.Fatalf("history: %+v", list)
	}

	// a ticket pulled from the backlog joins the open sprint
	bug, _ := ts.CreateBacklog(ctx, "s1", pid, "bug", "from backlog", "")
	if err := ts.Move(ctx, "s1", bug.ID, todo, 0); err != nil {
		t.Fatal(err)
	}
	if _, cols := view(); len(cols["To do"]) != 3 || cols["To do"][0] != "from backlog" {
		t.Fatalf("backlog → sprint: %+v", cols)
	}
}
