package sprints

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/boards"
	"github.com/tracklines/backend/internal/tickets"
)

// Closing a sprint by hand only closes it: the next one waits for Start, so its length can be chosen.
// Unfinished tickets wait on the board, and the planned next sprint (with its goal) joins at Start.
// Running out still rolls straight into the next sprint.
func TestManualCloseWaitsForStart(t *testing.T) {
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
	if _, err := db.Exec(ctx, `DELETE FROM boards WHERE owner_clerk_id = 'mc1'; DELETE FROM projects WHERE owner_clerk_id = 'mc1'`); err != nil {
		t.Fatal(err)
	}
	var pid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('mc1', 'p') RETURNING id`).Scan(&pid)
	bs, ts, s := boards.Store{DB: db}, tickets.Store{DB: db}, Store{DB: db}
	b, _ := bs.Create(ctx, "mc1", pid, "b", "")
	todo, done := b.Columns[0].ID, b.Columns[len(b.Columns)-1].ID
	open, _ := ts.Create(ctx, "mc1", todo, "task", "unfinished", "")
	fin, _ := ts.Create(ctx, "mc1", todo, "task", "finished", "")
	sp1, err := s.Start(ctx, "mc1", b.ID, 14)
	if err != nil {
		t.Fatal(err)
	}
	_ = ts.Move(ctx, "mc1", fin.ID, done, 0)
	// refinement planned the next sprint, with a goal and a backlog ticket
	if _, err := s.Plan(ctx, "mc1", b.ID, 7, "Ship search"); err != nil {
		t.Fatal(err)
	}
	plans, _ := s.Planned(ctx, "mc1", b.ID)
	backlogged, _ := ts.CreateBacklog(ctx, "mc1", pid, "task", "planned in", "")
	if err := s.PlanTicket(ctx, "mc1", backlogged.ID, &plans[0].ID, nil); err != nil {
		t.Fatal(err)
	}

	// epics stay in the backlog: they aren't planned into sprints, their sub-tickets are (TL-213)
	epicTk, _ := ts.CreateBacklog(ctx, "mc1", pid, "epic", "an epic", "")
	if err := s.PlanTicket(ctx, "mc1", epicTk.ID, &plans[0].ID, nil); !errors.Is(err, ErrEpicNotPlanned) {
		t.Fatalf("epic planned into a sprint: %v", err)
	}

	sprintOf := func(id string) *string {
		var sp *string
		_ = db.QueryRow(ctx, `SELECT sprint_id::text FROM tickets WHERE id = $1`, id).Scan(&sp)
		return sp
	}
	openSprints := func() int {
		var n int
		_ = db.QueryRow(ctx, `SELECT count(*) FROM sprints WHERE board_id = $1 AND closed_at IS NULL`, b.ID).Scan(&n)
		return n
	}

	// close by hand: nothing opens; unfinished waits on the board; done stays with sprint 1; the plan waits
	if next, err := s.CloseWithOptions(ctx, "mc1", sp1.ID, CloseOptions{}); err != nil || next != nil {
		t.Fatalf("manual close: %+v %v", next, err)
	}
	if n := openSprints(); n != 0 {
		t.Fatalf("%d sprint(s) open after a manual close", n)
	}
	if sp := sprintOf(open.ID); sp != nil {
		t.Fatalf("unfinished ticket in sprint %s", *sp)
	}
	if sp := sprintOf(fin.ID); sp == nil || *sp != sp1.ID {
		t.Fatal("finished ticket left sprint 1")
	}
	if ps, _ := s.Planned(ctx, "mc1", b.ID); len(ps) != 1 {
		t.Fatalf("plan used up by a close that started nothing: %d", len(ps))
	}
	view, _ := bs.Get(ctx, b.ID, "mc1")
	if view.Sprint != nil || len(view.Columns[0].Tickets) != 1 || view.Columns[0].Tickets[0].ID != open.ID {
		t.Fatalf("board after close: sprint %+v, to do %+v", view.Sprint, view.Columns[0].Tickets)
	}
	// burn and history still count what sprint 1 held
	if d, _ := s.Detail(ctx, "mc1", sp1.ID); d.CarriedOver == nil || *d.CarriedOver != 1 || d.Burn.Total != 2 {
		t.Fatalf("sprint 1 detail: %+v", d)
	}

	// Start with the length you choose: the waiting tickets and the planned sprint join, with its goal
	sp2, err := s.Start(ctx, "mc1", b.ID, 5)
	if err != nil || sp2.Number != 2 || sp2.LengthDays != 5 || sp2.Goal != "Ship search" {
		t.Fatalf("start after close: %+v %v", sp2, err)
	}
	if sp := sprintOf(open.ID); sp == nil || *sp != sp2.ID {
		t.Fatal("unfinished ticket didn't join the new sprint")
	}
	if sp := sprintOf(backlogged.ID); sp == nil || *sp != sp2.ID {
		t.Fatal("planned ticket didn't join the new sprint")
	}
	// joining from the plan is in its history too (TL-75): from the backlog into the first column
	h, err := ts.History(ctx, "mc1", backlogged.ID)
	if err != nil || len(h) != 2 || h[0].Stage != "backlog" || h[1].Stage != "column" || h[1].Outcome != "entered" || *h[1].ColumnID != todo {
		t.Fatalf("planned ticket's history: %+v %v", h, err)
	}
	if ps, _ := s.Planned(ctx, "mc1", b.ID); len(ps) != 0 {
		t.Fatalf("plan still there: %d", len(ps))
	}

	// running out still rolls straight into the next sprint
	_, _ = db.Exec(ctx, `UPDATE sprints SET starts_at = now() - interval '6 days', ends_at = now() - interval '1 minute' WHERE id = $1`, sp2.ID)
	if n, err := s.AutoClose(ctx); err != nil || n < 1 {
		t.Fatalf("auto-close: %d %v", n, err)
	}
	if n := openSprints(); n != 1 {
		t.Fatalf("%d sprint(s) open after auto-close", n)
	}
}
