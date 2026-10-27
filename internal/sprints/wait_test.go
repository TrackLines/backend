package sprints

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/boards"
	"github.com/tracklines/backend/internal/tickets"
)

// TL-279: a board set to wait ends a sprint that runs out and leaves the next for a manager to start,
// telling its managers; a board left on start_next goes straight into the next sprint, as before
func TestSprintEndsAndWaits(t *testing.T) {
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
	const org = "sw1"
	for _, q := range []string{`DELETE FROM boards WHERE owner_clerk_id = 'sw1'`, `DELETE FROM projects WHERE owner_clerk_id = 'sw1'`,
		`DELETE FROM org_admins WHERE org_id = 'sw1'`, `INSERT INTO org_admins (org_id, user_clerk_id, granted_by) VALUES ('sw1', 'user_admin', 'test')`} {
		if _, err := db.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	var pid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ($1, 'Wait') RETURNING id`, org).Scan(&pid)
	n := &recorder{}
	s := Store{DB: db, Notify: n}
	bs := boards.Store{DB: db}
	ts := tickets.Store{DB: db}
	board := func(name, when string) (boards.Board, string) {
		t.Helper()
		b, err := bs.Create(ctx, org, pid, name, "")
		if err != nil {
			t.Fatal(err)
		}
		if when != "" {
			if err := bs.SetWhenSprintEnds(ctx, b.ID, org, when); err != nil {
				t.Fatal(err)
			}
		}
		sp, err := s.Start(ctx, org, b.ID, 14)
		if err != nil {
			t.Fatal(err)
		}
		tk, err := ts.Create(ctx, org, b.Columns[0].ID, "task", "unfinished", "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(ctx, `UPDATE sprints SET starts_at = now() - interval '15 days', ends_at = now() - interval '1 minute' WHERE id = $1`, sp.ID); err != nil {
			t.Fatal(err)
		}
		return *b, tk.ID
	}
	waits, waitTicket := board("Waits", "wait")
	rolls, _ := board("Rolls", "")
	if _, err := s.AutoClose(ctx); err != nil {
		t.Fatal(err)
	}

	open := func(b string) *int {
		var number *int
		_ = db.QueryRow(ctx, `SELECT number FROM sprints WHERE board_id = $1 AND closed_at IS NULL`, b).Scan(&number)
		return number
	}
	if n := open(rolls.ID); n == nil || *n != 2 {
		t.Fatalf("start_next: open sprint %v, want 2", n)
	}
	if n := open(waits.ID); n != nil {
		t.Fatalf("wait: sprint %d started on its own", *n)
	}
	var inSprint *string
	_ = db.QueryRow(ctx, `SELECT sprint_id::text FROM tickets WHERE id = $1`, waitTicket).Scan(&inSprint)
	if inSprint != nil {
		t.Fatal("wait: the unfinished ticket is still in the closed sprint")
	}
	got, err := bs.Get(ctx, waits.ID, org)
	if err != nil || got.WhenSprintEnds != "wait" || got.LastSprintNumber == nil || *got.LastSprintNumber != 1 || got.Sprint != nil {
		t.Fatalf("waiting board: %+v %v", got, err)
	}
	if rolled, _ := bs.Get(ctx, rolls.ID, org); rolled.WhenSprintEnds != "start_next" {
		t.Fatalf("default: %q", rolled.WhenSprintEnds)
	}

	// the managers are told, once, and only about the board that waits
	var waiting []told
	for _, g := range n.got {
		if g.e.Kind == "sprint_waiting" {
			waiting = append(waiting, g)
		}
	}
	if len(waiting) != 1 || waiting[0].e.BoardID != waits.ID || strings.Join(waiting[0].to, ",") != "user_admin" ||
		!strings.Contains(waiting[0].e.Text, "ended sprint 1 on Waits: start sprint 2") {
		t.Fatalf("told: %+v", waiting)
	}

	// a manager starts it: sprint 2, and the waiting ticket joins
	sp, err := s.Start(ctx, org, waits.ID, 14)
	if err != nil || sp.Number != 2 {
		t.Fatalf("start after waiting: %+v %v", sp, err)
	}
	_ = db.QueryRow(ctx, `SELECT sprint_id::text FROM tickets WHERE id = $1`, waitTicket).Scan(&inSprint)
	if inSprint == nil || *inSprint != sp.ID {
		t.Fatal("the waiting ticket didn't join the next sprint")
	}
	if err := bs.SetWhenSprintEnds(ctx, waits.ID, org, "sometimes"); err == nil {
		t.Fatal("the database took a value that isn't start_next or wait")
	}
}
