package roadmaps

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TL-216: a milestone linked to an epic counts the epic's sub-tickets (not the epic), as they are on
// every read, so ones added later count too. The epic stays a single link.
func TestEpicLinks(t *testing.T) {
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
	if _, err := db.Exec(ctx, `DELETE FROM roadmaps WHERE owner_clerk_id = 're1'; DELETE FROM projects WHERE owner_clerk_id = 're1'`); err != nil {
		t.Fatal(err)
	}
	var pid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('re1', 'p') RETURNING id`).Scan(&pid)
	ticket := func(typ, title string, parent *string, done bool) string {
		var id string
		if err := db.QueryRow(ctx, `INSERT INTO tickets (project_id, type, title, position, created_by, parent_id, resolved_at)
			VALUES ($1, $2::ticket_type, $3, 0, 'u', $4, CASE WHEN $5 THEN now() END) RETURNING id`, pid, typ, title, parent, done).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	epic := ticket("epic", "Checkout", nil, false)
	c1 := ticket("task", "pay", &epic, true)
	ticket("task", "ship", &epic, false)
	dup := ticket("task", "pay again", &epic, false)
	_, _ = db.Exec(ctx, `UPDATE tickets SET duplicate_of = $2 WHERE id = $1`, dup, c1) // duplicates don't count
	other := ticket("bug", "unrelated fix", nil, false)

	s := Store{DB: db}
	rm, err := s.Create(ctx, "re1", pid, "R", "", "team")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceItems(ctx, rm.ID, "re1", []Item{{Title: "Checkout"}}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(ctx, rm.ID)
	item := got.Items[0].ID
	// linked: the epic, plus a ticket outside it, plus one of its sub-tickets directly (counted once)
	if err := s.SetItemTickets(ctx, "re1", rm.ID, item, []string{epic, other, c1}); err != nil {
		t.Fatal(err)
	}
	progress := func() (Progress, Progress, []LinkedTicket) {
		g, err := s.Get(ctx, rm.ID)
		if err != nil {
			t.Fatal(err)
		}
		return g.Items[0].Progress, g.Progress, g.Items[0].Tickets
	}
	p, total, links := progress()
	if p.Total != 3 || p.Done != 1 || total.Total != 3 { // pay (done), ship, unrelated fix
		t.Fatalf("progress: item %+v roadmap %+v", p, total)
	}
	types := map[string]string{}
	for _, l := range links {
		types[l.Title] = l.Type
	}
	if len(links) != 3 || types["Checkout"] != "epic" || types["unrelated fix"] != "bug" {
		t.Fatalf("links stay as linked: %+v", links)
	}
	// work added to the epic later counts without relinking
	ticket("feature", "receipts", &epic, true)
	if p, _, _ = progress(); p.Total != 4 || p.Done != 2 {
		t.Fatalf("after adding to the epic: %+v", p)
	}
	if p.Done == p.Total || got.Items[0].Status == "done" {
		t.Fatal("milestone finished early")
	}
}
