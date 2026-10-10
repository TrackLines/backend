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

func TestPlannedSprints(t *testing.T) {
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
	if _, err := db.Exec(ctx, `DELETE FROM boards WHERE owner_clerk_id = 'pl1'; DELETE FROM projects WHERE owner_clerk_id = 'pl1'`); err != nil {
		t.Fatal(err)
	}
	var pid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('pl1', 'p') RETURNING id`).Scan(&pid)
	bs := boards.Store{DB: db}
	b, _ := bs.Create(ctx, "pl1", pid, "fib", "")
	other, _ := bs.Create(ctx, "pl1", pid, "tshirt", "")
	if _, err := db.Exec(ctx, `UPDATE boards SET estimate_scale = 'fibonacci' WHERE id = $1; `, b.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `UPDATE boards SET estimate_scale = 'tshirt' WHERE id = $1`, other.ID); err != nil {
		t.Fatal(err)
	}
	s := Store{DB: db}
	ts := tickets.Store{DB: db}
	backlog := func(title string) string {
		tk, err := ts.CreateBacklog(ctx, "pl1", pid, "task", title, "")
		if err != nil {
			t.Fatal(err)
		}
		return tk.ID
	}
	plans := func() []PlannedSprint {
		ps, err := s.Planned(ctx, "pl1", b.ID)
		if err != nil {
			t.Fatal(err)
		}
		return ps
	}
	str := func(v string) *string { return &v }
	estimate := func(id string) string {
		var e *string
		_ = db.QueryRow(ctx, `SELECT estimate FROM tickets WHERE id = $1`, id).Scan(&e)
		if e == nil {
			return ""
		}
		return *e
	}

	// sprint 1 runs; two sprints can be planned after it, numbered 2 and 3
	sp1, err := s.Start(ctx, "pl1", b.ID, 14)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Plan(ctx, "pl1", b.ID, 7); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Plan(ctx, "pl1", b.ID, 14); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Plan(ctx, "pl1", b.ID, 14); !errors.Is(err, ErrTooManyPlanned) {
		t.Fatalf("third plan: %v", err)
	}
	ps := plans()
	if len(ps) != 2 || ps[0].Number != 2 || ps[1].Number != 3 || ps[0].LengthDays != 7 {
		t.Fatalf("plans: %+v", ps)
	}
	next, later := ps[0].ID, ps[1].ID

	// no closed sprints yet: velocity unknown, so no cap and nothing needs approval
	a, bb, c := backlog("a"), backlog("b"), backlog("c")
	if err := s.PlanTicket(ctx, "pl1", a, &next, str("4")); !errors.Is(err, boards.ErrBadEstimate) {
		t.Fatalf("off-scale estimate: %v", err)
	}
	for id, e := range map[string]string{a: "5", bb: "3"} {
		if err := s.PlanTicket(ctx, "pl1", id, &next, str(e)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.PlanTicket(ctx, "pl1", c, &next, nil); err != nil { // unestimated
		t.Fatal(err)
	}
	cp := plans()[0].Capacity
	if cp.Unit != "points" || cp.Velocity != nil || cp.Cap != nil || cp.Used != 8 || cp.Unestimated != 1 || cp.Over || cp.NeedsApproval {
		t.Fatalf("no history: %+v", cp)
	}
	// the planned ticket is still in the backlog, now with an estimate on the board's scale
	if d, _ := ts.Get(ctx, "pl1", a); d.BoardID != nil || d.PlannedSprintID == nil || *d.PlannedSprintID != next || d.EstimateScale != "fibonacci" {
		t.Fatalf("planned ticket: %+v", d)
	}
	// only backlog tickets can be planned
	onBoard, _ := ts.Create(ctx, "pl1", b.Columns[0].ID, "task", "on board", "")
	if err := s.PlanTicket(ctx, "pl1", onBoard.ID, &next, str("1")); !errors.Is(err, ErrNotBacklog) {
		t.Fatalf("board ticket: %v", err)
	}

	// history: three closed sprints that finished 10, 6 and 8 points → velocity 8, cap 8.8
	for i, pts := range []string{"5,5", "3,3", "8"} {
		var id string
		_ = db.QueryRow(ctx, `INSERT INTO sprints (board_id, number, length_days, starts_at, ends_at, closed_at)
			VALUES ($1, $2, 14, now() - interval '60 days', now() - interval '50 days', now() - interval '50 days') RETURNING id`,
			other.ID, 100+i).Scan(&id) // on another board first, to check it's ignored
		_ = db.QueryRow(ctx, `INSERT INTO sprints (board_id, number, length_days, starts_at, ends_at, closed_at)
			VALUES ($1, $2, 14, now() - interval '60 days', now() - interval '50 days', now() - interval '50 days') RETURNING id`,
			b.ID, -10+i).Scan(&id)
		for _, e := range splitComma(pts) {
			if _, err := db.Exec(ctx, `INSERT INTO tickets (project_id, board_id, column_id, sprint_id, type, title, position, created_by, estimate)
				VALUES ($1, $2, $3, $4, 'task', 'done', 0, 'pl1', $5)`, pid, b.ID, b.Columns[2].ID, id, e); err != nil {
				t.Fatal(err)
			}
		}
	}
	cp = plans()[0].Capacity
	if cp.Velocity == nil || *cp.Velocity != 8 || cp.Cap == nil || *cp.Cap != 8.8 || cp.Over {
		t.Fatalf("with history: %+v", cp)
	}
	// over the cap: needs approval; approving covers today's total; adding more needs it again
	d := backlog("d")
	if err := s.PlanTicket(ctx, "pl1", d, &next, str("2")); err != nil {
		t.Fatal(err)
	}
	if cp = plans()[0].Capacity; !cp.Over || !cp.NeedsApproval || cp.Used != 10 {
		t.Fatalf("over: %+v", cp)
	}
	approved, err := s.Approve(ctx, "pl1", "user_lead", next)
	if err != nil || approved.Capacity.NeedsApproval || *approved.Capacity.ApprovedTotal != 10 || *approved.Capacity.ApprovedBy != "user_lead" {
		t.Fatalf("approve: %+v %v", approved, err)
	}
	if err := s.PlanTicket(ctx, "pl1", c, &next, str("1")); err != nil {
		t.Fatal(err)
	}
	if cp = plans()[0].Capacity; !cp.NeedsApproval {
		t.Fatalf("more after approval: %+v", cp)
	}

	// closing sprint 1 while plan 1 needs approval: not promoted, it stays planned
	sp2, err := s.Close(ctx, "pl1", sp1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ps = plans(); len(ps) != 2 || ps[0].ID != next || sp2.LengthDays != 14 {
		t.Fatalf("unapproved plan promoted: %+v %+v", ps, sp2)
	}
	if d2, _ := ts.Get(ctx, "pl1", a); d2.BoardID != nil {
		t.Fatal("unapproved plan's tickets joined the sprint")
	}
	// trimmed back under the approval: closing promotes it with its length, tickets and estimates
	if err := s.PlanTicket(ctx, "pl1", c, nil, nil); err != nil {
		t.Fatal(err)
	}
	if estimate(c) != "" {
		t.Fatal("unplanning keeps an estimate the backlog can't hold")
	}
	sp3, err := s.Close(ctx, "pl1", sp2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sp3.LengthDays != 7 || sp3.Number != sp2.Number+1 {
		t.Fatalf("promoted sprint: %+v", sp3)
	}
	for _, id := range []string{a, bb, d} {
		got, _ := ts.Get(ctx, "pl1", id)
		if got.BoardID == nil || *got.BoardID != b.ID || got.SprintID == nil || *got.SprintID != sp3.ID ||
			*got.ColumnID != b.Columns[0].ID || got.PlannedSprintID != nil || got.Estimate == nil {
			t.Fatalf("promoted ticket: %+v", got)
		}
	}
	if estimate(a) != "5" {
		t.Fatal("promotion lost the estimate")
	}
	if ps = plans(); len(ps) != 1 || ps[0].ID != later || ps[0].Position != 1 || ps[0].Number != sp3.Number+1 {
		t.Fatalf("later plan moves up: %+v", ps)
	}

	// planning onto another board's sprint: a different scale clears the estimate
	e := backlog("e")
	if err := s.PlanTicket(ctx, "pl1", e, &later, str("8")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Plan(ctx, "pl1", other.ID, 14); err != nil {
		t.Fatal(err)
	}
	op, _ := s.Planned(ctx, "pl1", other.ID)
	if err := s.PlanTicket(ctx, "pl1", e, &op[0].ID, nil); err != nil || estimate(e) != "" {
		t.Fatalf("other scale: %v %q", err, estimate(e))
	}
	if err := s.PlanTicket(ctx, "pl1", e, &op[0].ID, str("M")); err != nil || estimate(e) != "M" {
		t.Fatalf("t-shirt estimate: %v %q", err, estimate(e))
	}
	// moving a planned ticket onto its board by hand leaves the plan and keeps the estimate
	if err := ts.MoveEstimated(ctx, "pl1", e, other.Columns[0].ID, 0, nil, true); err != nil || estimate(e) != "M" {
		t.Fatalf("move planned ticket: %v %q", err, estimate(e))
	}
	if got, _ := ts.Get(ctx, "pl1", e); got.PlannedSprintID != nil {
		t.Fatal("moved ticket still planned")
	}

	// changing the board's scale clears planned estimates too
	f := backlog("f")
	if err := s.PlanTicket(ctx, "pl1", f, &later, str("3")); err != nil {
		t.Fatal(err)
	}
	if err := bs.SetEstimateScale(ctx, b.ID, "pl1", "linear"); err != nil {
		t.Fatal(err)
	}
	if estimate(f) != "" {
		t.Fatal("scale change kept a planned estimate")
	}

	// removing a plan: its tickets go back to the plain backlog
	if err := s.Unplan(ctx, "pl1", later); err != nil {
		t.Fatal(err)
	}
	if got, _ := ts.Get(ctx, "pl1", f); got.PlannedSprintID != nil || len(plans()) != 0 {
		t.Fatalf("unplan: %+v", got)
	}

	// kanban boards don't plan sprints
	if _, err := db.Exec(ctx, `UPDATE boards SET style = 'kanban' WHERE id = $1`, other.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Plan(ctx, "pl1", other.ID, 7); !errors.Is(err, ErrKanban) {
		t.Fatalf("kanban: %v", err)
	}
}

func splitComma(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return out
}
