package tickets

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/boards"
)

func TestResolve(t *testing.T) {
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
	if _, err := db.Exec(ctx, `DELETE FROM boards WHERE owner_clerk_id = 'rs1'; DELETE FROM projects WHERE owner_clerk_id = 'rs1'`); err != nil {
		t.Fatal(err)
	}
	var pid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('rs1', 'p') RETURNING id`).Scan(&pid)
	b, _ := boards.Store{DB: db}.Create(ctx, "rs1", pid, "b", "")
	todo, done := b.Columns[0].ID, b.Columns[2].ID
	s := Store{DB: db}
	backlog := func(title string) string { tk, _ := s.CreateBacklog(ctx, "rs1", pid, "task", title, ""); return tk.ID }
	onBoard := func(title string) string { tk, _ := s.Create(ctx, "rs1", todo, "task", title, ""); return tk.ID }
	child := func(id, parent string) {
		if err := s.SetParent(ctx, "rs1", id, &parent); err != nil {
			t.Fatal(err)
		}
	}
	isDone := func(id string) bool {
		d, err := s.Get(ctx, "rs1", id)
		if err != nil {
			t.Fatal(err)
		}
		return d.Done
	}
	open := func() map[string]bool {
		ts, _ := s.OpenTickets(ctx, "rs1", pid, OpenFilter{})
		out := map[string]bool{}
		for _, t := range ts {
			out[t.ID] = true
		}
		return out
	}

	// resolving a backlog ticket: done, out of the backlog and open lists, still readable; repeat is a no-op
	solo := backlog("solo")
	if err := s.Resolve(ctx, "rs1", solo); err != nil {
		t.Fatal(err)
	}
	if err := s.Resolve(ctx, "rs1", solo); err != nil {
		t.Fatal("repeat:", err)
	}
	if d, _ := s.Get(ctx, "rs1", solo); !d.Done || d.ResolvedAt == nil || d.DoneAt == nil || d.CreatedAt == "" || d.UpdatedAt == "" {
		t.Fatalf("resolved detail (with dates): %+v", d)
	}
	if page, _ := s.PageBacklog(ctx, "rs1", pid, "", nil, 1, 100); len(page.Tickets) != 0 || page.Counts["all"] != 0 {
		t.Fatalf("backlog still shows it: %+v", page)
	}
	if open()[solo] {
		t.Fatal("open tickets still list it")
	}
	// a board ticket closes into its Done column (TL-272: Bugfixes' resolve and ignore), done or won't do;
	// moving it out of Done reopens it. AI agents still move their own work (a board's review stands).
	fixed, ignored := onBoard("fixed"), onBoard("ignored")
	if err := s.Resolve(ctx, "rs1", fixed); err != nil {
		t.Fatal("board ticket:", err)
	}
	if err := s.ResolveAs(ctx, "rs1", "Bugfixes", ignored, ResolutionWontDo, "ignored in Bugfixes"); err != nil {
		t.Fatal("board ticket, won't do:", err)
	}
	for id, want := range map[string]string{fixed: ResolutionDone, ignored: ResolutionWontDo} {
		d, _ := s.Get(ctx, "rs1", id)
		if !d.Done || d.ColumnID == nil || *d.ColumnID != done || d.Resolution == nil || *d.Resolution != want {
			t.Fatalf("closed on board as %s: %+v", want, d)
		}
	}
	var reason int
	_ = s.DB.QueryRow(ctx, `SELECT count(*) FROM ticket_comments WHERE ticket_id = $1 AND author = 'Bugfixes' AND body = 'Won''t do: ignored in Bugfixes'`, ignored).Scan(&reason)
	if reason != 1 {
		t.Fatalf("won't do reason comment: %d", reason)
	}
	if err := s.Move(ctx, "rs1", ignored, todo, 0); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.Get(ctx, "rs1", ignored); d.Done || d.Resolution != nil || d.ResolvedAt != nil {
		t.Fatalf("reopened from Done: %+v", d)
	}
	if err := s.ResolveAsAgent(ctx, "rs1", "claude", onBoard("agent's"), ResolutionDone, "", true); !errors.Is(err, ErrOnBoard) {
		t.Fatal("an agent resolving a board ticket:", err)
	}
	if err := s.Resolve(ctx, "other", solo); !errors.Is(err, ErrNotFound) {
		t.Fatal("other org:", err)
	}
	// a resolved blocker no longer blocks
	waiting := backlog("waiting")
	_ = s.SetBlockedBy(ctx, "rs1", waiting, []string{solo})
	if d, _ := s.Get(ctx, "rs1", waiting); d.Blocked {
		t.Fatal("a resolved blocker should count as done")
	}

	// backlog parent: finishes when its last child does, by a mix of resolving and Done
	parent := backlog("parent")
	a, bb := backlog("a"), onBoard("b")
	child(a, parent)
	child(bb, parent)
	_ = s.Resolve(ctx, "rs1", a)
	if isDone(parent) {
		t.Fatal("parent done with a child still open")
	}
	if err := s.Move(ctx, "rs1", bb, done, 0); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.Get(ctx, "rs1", parent); !d.Done || d.ResolvedAt == nil {
		t.Fatalf("backlog parent should be resolved: %+v", d)
	}

	// board parent moves to Done; and it carries up to a backlog grandparent
	grand := backlog("grand")
	boardParent := onBoard("board parent")
	child(boardParent, grand)
	k := backlog("k")
	child(k, boardParent)
	_ = s.Resolve(ctx, "rs1", k)
	if d, _ := s.Get(ctx, "rs1", boardParent); d.ColumnID == nil || *d.ColumnID != done {
		t.Fatalf("board parent should be in Done: %+v", d)
	}
	if !isDone(grand) {
		t.Fatal("grandparent should complete too")
	}

	// a ticket with no children isn't auto-completed; reopening: moving a resolved ticket onto a board
	if isDone(waiting) {
		t.Fatal("childless ticket completed itself")
	}
	if err := s.Move(ctx, "rs1", solo, todo, 0); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.Get(ctx, "rs1", solo); d.Done || d.ResolvedAt != nil || d.DoneAt != nil {
		t.Fatalf("reopened: %+v", d)
	}
}
