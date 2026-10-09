package sprints

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/boards"
	"github.com/tracklines/backend/internal/columns"
	"github.com/tracklines/backend/internal/tickets"
)

// Needs a throwaway db: TEST_DATABASE_URL=postgres://... go test ./internal/sprints/
func TestKanban(t *testing.T) {
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
	if _, err := db.Exec(ctx, `DELETE FROM boards WHERE owner_clerk_id = 'kb1'; DELETE FROM projects WHERE owner_clerk_id = 'kb1'`); err != nil {
		t.Fatal(err)
	}
	var pid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('kb1', 'p') RETURNING id`).Scan(&pid)
	bs, ts, cs, s := boards.Store{DB: db}, tickets.Store{DB: db}, columns.Store{DB: db}, Store{DB: db}
	b, _ := bs.Create(ctx, "kb1", pid, "b", "")
	todo, doing, done := b.Columns[0].ID, b.Columns[1].ID, b.Columns[2].ID

	// no switching mid-sprint
	sp, _ := s.Start(ctx, "kb1", b.ID, 7)
	if err := bs.SetStyle(ctx, b.ID, "kb1", "kanban"); !errors.Is(err, boards.ErrSprintOpen) {
		t.Fatal("kanban with an open sprint:", err)
	}
	if _, err := s.Close(ctx, "kb1", sp.ID); err != nil {
		t.Fatal(err)
	}
	_, _ = db.Exec(ctx, `UPDATE sprints SET closed_at = now() WHERE board_id = $1 AND closed_at IS NULL`, b.ID) // close the follow-on sprint too
	if err := bs.SetStyle(ctx, b.ID, "kb1", "kanban"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Start(ctx, "kb1", b.ID, 7); !errors.Is(err, ErrKanban) {
		t.Fatal("sprint on a kanban board:", err)
	}
	if err := bs.SetStyle(ctx, b.ID, "other", "sprints"); !errors.Is(err, boards.ErrNotFound) {
		t.Fatal("other owner:", err)
	}

	// WIP limits: set, read back, clear
	if err := cs.SetWIPLimit(ctx, "kb1", doing, 3); err != nil {
		t.Fatal(err)
	}
	if err := cs.SetWIPLimit(ctx, "other", doing, 1); !errors.Is(err, columns.ErrNotFound) {
		t.Fatal("other owner wip:", err)
	}

	// Done keeps recent work only
	fresh, _ := ts.Create(ctx, "kb1", todo, "task", "fresh", "")
	old, _ := ts.Create(ctx, "kb1", todo, "task", "old", "")
	_ = ts.Move(ctx, "kb1", fresh.ID, done, 0)
	_ = ts.Move(ctx, "kb1", old.ID, done, 0)
	_, _ = db.Exec(ctx, `UPDATE tickets SET done_at = now() - interval '30 days' WHERE id = $1`, old.ID)
	full, err := bs.Get(ctx, b.ID, "kb1")
	if err != nil || full.Style != "kanban" || full.HiddenDone != 1 || full.Columns[1].WIPLimit == nil || *full.Columns[1].WIPLimit != 3 {
		t.Fatalf("board: %+v %v", full, err)
	}
	if ds := full.Columns[2].Tickets; len(ds) != 1 || ds[0].ID != fresh.ID {
		t.Fatalf("done column: %+v", ds)
	}
	_ = cs.SetWIPLimit(ctx, "kb1", doing, 0)
	if full, _ = bs.Get(ctx, b.ID, "kb1"); full.Columns[1].WIPLimit != nil {
		t.Fatal("wip limit should clear")
	}
	// back to sprints: the whole Done column shows again
	_ = bs.SetStyle(ctx, b.ID, "kb1", "sprints")
	if full, _ = bs.Get(ctx, b.ID, "kb1"); full.HiddenDone != 0 || len(full.Columns[2].Tickets) != 2 {
		t.Fatalf("sprints view: hidden %d, done %d", full.HiddenDone, len(full.Columns[2].Tickets))
	}
}
