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

// PostgreSQL is provisioned by this package's TestMain.
func TestVelocity(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Fatal("TEST_DATABASE_URL not set; package TestMain should provision PostgreSQL")
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
	if _, err := db.Exec(ctx, `DELETE FROM boards WHERE owner_clerk_id = 'v1'; DELETE FROM projects WHERE owner_clerk_id = 'v1'`); err != nil {
		t.Fatal(err)
	}
	var pid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('v1', 'p') RETURNING id`).Scan(&pid)
	bs, ts, s := boards.Store{DB: db}, tickets.Store{DB: db}, Store{DB: db}
	b, _ := bs.Create(ctx, "v1", pid, "b", "")
	_ = bs.SetEstimateScale(ctx, b.ID, "v1", "fibonacci")
	todo, done := b.Columns[0].ID, b.Columns[2].ID
	add := func(est string) string {
		tk, err := ts.Create(ctx, "v1", todo, "task", est, "")
		if err != nil {
			t.Fatal(err)
		}
		if est != "" {
			if err := ts.SetEstimate(ctx, "v1", tk.ID, est); err != nil {
				t.Fatal(err)
			}
		}
		return tk.ID
	}
	doneAt := func(id string) bool {
		var at *string
		_ = db.QueryRow(ctx, `SELECT done_at::text FROM tickets WHERE id = $1`, id).Scan(&at)
		return at != nil
	}

	if v, err := s.Velocity(ctx, "v1", b.ID); err != nil || v.Unit != "points" || len(v.Sprints) != 0 || v.Current != nil || v.Cap != nil { // no history: no limit
		t.Fatalf("no sprints yet: %+v %v", v, err)
	}
	five, three := add("5"), add("3")
	sp, _ := s.Start(ctx, "v1", b.ID, 14)
	if err := ts.Move(ctx, "v1", five, done, 0); err != nil || !doneAt(five) {
		t.Fatal("into done stamps done_at:", err)
	}
	if _, err := s.Close(ctx, "v1", sp.ID); err != nil { // three carries over into sprint 2
		t.Fatal(err)
	}
	add("") // unestimated, joins sprint 2
	v, err := s.Velocity(ctx, "v1", b.ID)
	if err != nil || len(v.Sprints) != 1 || v.Sprints[0].Number != 1 || v.Sprints[0].Completed != 5 {
		t.Fatalf("velocity: %+v %v", v, err)
	}
	// planned: what sprint 1 held at close (5 + 3), beside what it finished, for the past sprints list
	if p := v.Sprints[0].Planned; p == nil || *p != 8 {
		t.Fatalf("planned: %v", p)
	}
	if v.Cap == nil || *v.Cap != 5.5 { // one closed sprint of 5 points, + 10% (TL-464)
		t.Fatalf("cap: %v", v.Cap)
	}
	if c := v.Current; c == nil || c.Number != 2 || c.Total != 3 || c.Unestimated != 1 || len(c.Done) != 0 {
		t.Fatalf("burn: %+v", c)
	}
	_ = ts.Move(ctx, "v1", three, done, 0)
	if v, _ = s.Velocity(ctx, "v1", b.ID); len(v.Current.Done) != 1 || v.Current.Done[0].Value != 3 || v.Current.Done[0].At == "" {
		t.Fatalf("burn after done: %+v", v.Current)
	}
	_ = ts.Move(ctx, "v1", three, todo, 0)
	if doneAt(three) {
		t.Fatal("out of done should clear done_at")
	}
	// no scale: count tickets; closed sprints keep their estimates but are counted as 1 each
	_ = bs.SetEstimateScale(ctx, b.ID, "v1", "none")
	if v, _ = s.Velocity(ctx, "v1", b.ID); v.Unit != "tickets" || v.Sprints[0].Completed != 1 || v.Current.Total != 2 {
		t.Fatalf("tickets unit: %+v %+v", v, v.Current)
	}
	if p := v.Sprints[0].Planned; p == nil || *p != 2 { // two tickets
		t.Fatalf("planned tickets: %v", p)
	}
	// closed before scope was recorded: planned unknown, not 0
	_, _ = db.Exec(ctx, `UPDATE sprints SET scope_tickets = NULL, scope_points = NULL WHERE id = $1`, sp.ID)
	if v, _ = s.Velocity(ctx, "v1", b.ID); v.Sprints[0].Planned != nil {
		t.Fatalf("unrecorded planned: %v", *v.Sprints[0].Planned)
	}
	if _, err := s.Velocity(ctx, "other", b.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("other owner:", err)
	}
}
