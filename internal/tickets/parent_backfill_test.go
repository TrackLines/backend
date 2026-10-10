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

// Parents whose sub-tickets all finished before auto-completion existed, and parents left with only
// finished sub-tickets when an unfinished one goes away (delete, detach), complete too.
func TestFinishedParents(t *testing.T) {
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
	if _, err := db.Exec(ctx, `DELETE FROM boards WHERE owner_clerk_id = 'fp1'; DELETE FROM projects WHERE owner_clerk_id = 'fp1'`); err != nil {
		t.Fatal(err)
	}
	var pid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('fp1', 'p') RETURNING id`).Scan(&pid)
	b, _ := boards.Store{DB: db}.Create(ctx, "fp1", pid, "b", "")
	todo, done := b.Columns[0].ID, b.Columns[2].ID
	s := Store{DB: db}
	// rows written straight to the table, as they were before auto-completion: no hooks run
	ticket := func(title string, parent *string, column *string, resolved bool) string {
		var id string
		var board *string
		if column != nil {
			board = &b.ID
		}
		if err := db.QueryRow(ctx, `INSERT INTO tickets (project_id, board_id, column_id, parent_id, type, title, position, created_by, resolved_at)
			VALUES ($1, $2, $3, $4, 'task', $5, 0, 'fp1', CASE WHEN $6 THEN now() END) RETURNING id`,
			pid, board, column, parent, title, resolved).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	isDone := func(id string) bool {
		d, err := s.Get(ctx, "fp1", id)
		if err != nil {
			t.Fatal(err)
		}
		return d.Done
	}

	// the backfill: a backlog epic over a board parent whose sub-tickets are all finished
	epic := ticket("epic", nil, nil, false)
	boardParent := ticket("board parent", &epic, &todo, false)
	ticket("done on board", &boardParent, &done, false)
	ticket("resolved in backlog", &boardParent, nil, true)
	open := ticket("still open parent", nil, nil, false)
	ticket("open child", &open, &todo, false)
	childless := ticket("no children", nil, nil, false)

	sql, err := os.ReadFile("../migrations/000032_complete_finished_parents.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 { // safe to run again
		if _, err := db.Exec(ctx, string(sql)); err != nil {
			t.Fatal(err)
		}
	}
	if !isDone(boardParent) || !isDone(epic) {
		t.Fatal("finished parents (and their parent, up the chain) weren't completed")
	}
	if d, _ := s.Get(ctx, "fp1", boardParent); d.ColumnID == nil || *d.ColumnID != done {
		t.Fatal("a board parent completes by moving to its board's Done column")
	}
	if d, _ := s.Get(ctx, "fp1", epic); d.ResolvedAt == nil {
		t.Fatal("a backlog parent completes by being resolved")
	}
	if isDone(open) || isDone(childless) {
		t.Fatal("a parent with unfinished or no sub-tickets was completed")
	}

	// deleting the last unfinished sub-ticket completes the parent
	p1 := ticket("p1", nil, nil, false)
	ticket("p1 done", &p1, &done, false)
	p1open := ticket("p1 open", &p1, &todo, false)
	if err := s.Delete(ctx, "fp1", p1open); err != nil || !isDone(p1) {
		t.Fatalf("delete: %v done=%v", err, isDone(p1))
	}
	// detaching it does too
	p2 := ticket("p2", nil, nil, false)
	ticket("p2 done", &p2, &done, false)
	p2open := ticket("p2 open", &p2, &todo, false)
	if err := s.SetParent(ctx, "fp1", p2open, nil); err != nil || !isDone(p2) {
		t.Fatalf("detach: %v done=%v", err, isDone(p2))
	}
	// adding a finished sub-ticket never completes the new parent
	p3 := ticket("p3", nil, nil, false)
	finished := ticket("finished elsewhere", nil, &done, false)
	if err := s.SetParent(ctx, "fp1", finished, &p3); err != nil || isDone(p3) {
		t.Fatalf("attach: %v done=%v", err, isDone(p3))
	}
}
