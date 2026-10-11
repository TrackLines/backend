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
)

func TestSprintGoal(t *testing.T) {
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
	if _, err := db.Exec(ctx, `DELETE FROM boards WHERE owner_clerk_id = 'gl1'; DELETE FROM projects WHERE owner_clerk_id = 'gl1'`); err != nil {
		t.Fatal(err)
	}
	var pid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('gl1', 'p') RETURNING id`).Scan(&pid)
	bs := boards.Store{DB: db}
	b, _ := bs.Create(ctx, "gl1", pid, "b", "")
	s := Store{DB: db}

	sp, err := s.Start(ctx, "gl1", b.ID, 14)
	if err != nil || sp.Goal != "" {
		t.Fatalf("start: %+v %v", sp, err)
	}
	// set, tidied to one line; too long refused; shown on the board
	if sp, err = s.SetGoal(ctx, "gl1", sp.ID, "  Ship   search\n and keys "); err != nil || sp.Goal != "Ship search and keys" {
		t.Fatalf("set goal: %+v %v", sp, err)
	}
	if _, err := s.SetGoal(ctx, "gl1", sp.ID, strings.Repeat("x", 201)); !errors.Is(err, ErrInvalidGoal) {
		t.Fatalf("long goal: %v", err)
	}
	if _, err := s.SetGoal(ctx, "other", sp.ID, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other org: %v", err)
	}
	if got, _ := bs.Get(ctx, b.ID, "gl1"); got.Sprint == nil || got.Sprint.Goal != "Ship search and keys" {
		t.Fatalf("board sprint goal: %+v", got.Sprint)
	}

	// planned sprints carry a goal into the sprint they become
	plans, err := s.Plan(ctx, "gl1", b.ID, 7, "Bugfixes panel live")
	if err != nil || plans[0].Goal != "Bugfixes panel live" {
		t.Fatalf("plan: %+v %v", plans, err)
	}
	goal, days := "Bugfixes panel + sprint goal UI", 10
	if plans, err = s.UpdatePlanned(ctx, "gl1", plans[0].ID, &days, &goal); err != nil || plans[0].Goal != goal || plans[0].LengthDays != 10 {
		t.Fatalf("update plan: %+v %v", plans, err)
	}
	next, err := s.Close(ctx, "gl1", sp.ID)
	if err != nil || next.Goal != goal || next.LengthDays != 10 {
		t.Fatalf("promoted sprint keeps the goal: %+v %v", next, err)
	}
	// with nothing planned, the next sprint starts without a goal
	after, err := s.Close(ctx, "gl1", next.ID)
	if err != nil || after.Goal != "" {
		t.Fatalf("unplanned next sprint: %+v %v", after, err)
	}
	// past sprints keep theirs
	if list, _ := s.List(ctx, "gl1", b.ID); len(list) != 3 || list[1].Goal != goal || list[2].Goal != "Ship search and keys" {
		t.Fatalf("history: %+v", list)
	}
}
