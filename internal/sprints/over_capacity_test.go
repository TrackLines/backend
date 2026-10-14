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
	"github.com/tracklines/backend/internal/notifications"
	"github.com/tracklines/backend/internal/tickets"
)

type told struct {
	to []string
	e  notifications.Event
}
type recorder struct{ got []told }

func (r *recorder) Notify(_ context.Context, _ string, to []string, e notifications.Event) error {
	r.got = append(r.got, told{to, e})
	return nil
}

// A sprint that starts over capacity (velocity + 10%) through carry-over tells the board's managers
// (TL-149), when it closes on its own or by hand; within capacity, or approved for that much, it doesn't.
func TestSprintStartsOverCapacity(t *testing.T) {
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
	const org = "oc1"
	for _, q := range []string{`DELETE FROM boards WHERE owner_clerk_id = 'oc1'`, `DELETE FROM projects WHERE owner_clerk_id = 'oc1'`,
		`DELETE FROM org_admins WHERE org_id = 'oc1'`, `INSERT INTO org_admins (org_id, user_clerk_id, granted_by) VALUES ('oc1', 'user_admin', 'test')`} {
		if _, err := db.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	var pid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ($1, 'Cap') RETURNING id`, org).Scan(&pid)
	n := &recorder{}
	s := Store{DB: db, Notify: n}
	ts := tickets.Store{DB: db}

	// a points board whose sprint 1 finished `done` points and leaves `left` points unfinished
	board := func(name string, done, left []string) (boards.Board, Sprint) {
		t.Helper()
		b, err := boards.Store{DB: db}.Create(ctx, org, pid, name, "")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = db.Exec(ctx, `UPDATE boards SET estimate_scale = 'fibonacci' WHERE id = $1`, b.ID)
		sp, err := s.Start(ctx, org, b.ID, 14)
		if err != nil {
			t.Fatal(err)
		}
		add := func(points string, finish bool) {
			tk, err := ts.Create(ctx, org, b.Columns[0].ID, "task", "work", "")
			if err != nil {
				t.Fatal(err)
			}
			_, _ = db.Exec(ctx, `UPDATE tickets SET estimate = $2 WHERE id = $1`, tk.ID, points)
			if finish {
				if err := ts.Move(ctx, org, tk.ID, b.Columns[len(b.Columns)-1].ID, 0); err != nil {
					t.Fatal(err)
				}
			}
		}
		for _, p := range done {
			add(p, true)
		}
		for _, p := range left {
			add(p, false)
		}
		return *b, *sp
	}

	// finished 5, carries 8 into sprint 2: over 5.5; closing on its own tells the admin
	a, aSprint := board("Backend", []string{"5"}, []string{"5", "3"})
	_, _ = db.Exec(ctx, `UPDATE sprints SET ends_at = now() - interval '1 minute' WHERE id = $1`, aSprint.ID)
	if closed, err := s.AutoClose(ctx); err != nil || closed != 1 {
		t.Fatalf("auto-close: %d %v", closed, err)
	}
	if len(n.got) != 1 || n.got[0].e.Kind != "over_capacity" || n.got[0].e.BoardID != a.ID || len(n.got[0].to) != 1 || n.got[0].to[0] != "user_admin" ||
		!strings.Contains(n.got[0].e.Text, "sprint 2 on Backend over capacity: 8 points against a limit of 5.5") {
		t.Fatalf("over capacity: %+v", n.got)
	}

	// finished 10, carries 3: within 11, closed by hand: nothing to say
	_, bSprint := board("Web", []string{"5", "5"}, []string{"3"})
	if _, err := s.Close(ctx, org, bSprint.ID); err != nil {
		t.Fatal(err)
	}
	if len(n.got) != 1 {
		t.Fatalf("within capacity, still told: %+v", n.got[1:])
	}

	// over, but a lead approved the plan for that much: their call, nobody is told again
	c, cSprint := board("Mobile", []string{"2"}, []string{"3"}) // carries 3 against 2.2
	plan, err := s.Plan(ctx, org, c.ID, 14, "")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = db.Exec(ctx, `UPDATE planned_sprints SET approved_total = 10, approved_by = 'user_admin', approved_at = now() WHERE id = $1`, plan[0].ID)
	if _, err := s.Close(ctx, org, cSprint.ID); err != nil {
		t.Fatal(err)
	}
	if len(n.got) != 1 {
		t.Fatalf("approved plan, still told: %+v", n.got[1:])
	}
}
