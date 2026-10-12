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

// TL-154: never-used boards can be deleted; used ones are archived (history kept), and restored.
func TestBoardArchive(t *testing.T) {
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
	if _, err := db.Exec(ctx, `DELETE FROM boards WHERE owner_clerk_id = 'ba1'; DELETE FROM projects WHERE owner_clerk_id = 'ba1'`); err != nil {
		t.Fatal(err)
	}
	var pid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('ba1', 'Archive') RETURNING id`).Scan(&pid)
	bs := boards.Store{DB: db}
	s := Store{DB: db}
	listed := func() int {
		list, _ := bs.ListByProject(ctx, pid)
		return len(list)
	}

	// never used: deletable, and deleted
	unused, _ := bs.Create(ctx, "ba1", pid, "Spare", "")
	if got, _ := bs.Get(ctx, unused.ID, "ba1"); !got.Deletable {
		t.Fatal("a new board should be deletable")
	}
	if err := bs.Delete(ctx, unused.ID, "ba1"); err != nil {
		t.Fatal(err)
	}

	// used: a ticket and a running sprint; can't be deleted or archived while work is open
	b, _ := bs.Create(ctx, "ba1", pid, "Web", "")
	var sprint string
	_ = db.QueryRow(ctx, `INSERT INTO sprints (board_id, number, length_days, starts_at, ends_at) VALUES ($1, 1, 14, now(), now() + interval '14 days') RETURNING id`, b.ID).Scan(&sprint)
	open, _ := s.Create(ctx, "ba1", b.Columns[0].ID, "task", "open work", "")
	if got, _ := bs.Get(ctx, b.ID, "ba1"); got.Deletable {
		t.Fatal("a used board is not deletable")
	}
	if err := bs.Delete(ctx, b.ID, "ba1"); !errors.Is(err, boards.ErrBoardUsed) {
		t.Fatalf("delete used: %v", err)
	}
	if err := bs.Archive(ctx, b.ID, "ba1"); !errors.Is(err, boards.ErrOpenTickets) {
		t.Fatalf("archive with open work: %v", err)
	}
	// finish it; a backlog ticket is planned into the board's next sprint
	_ = s.Move(ctx, "ba1", open.ID, b.Columns[2].ID, 0)
	var plan string
	_ = db.QueryRow(ctx, `INSERT INTO planned_sprints (board_id, position, length_days) VALUES ($1, 1, 14) RETURNING id`, b.ID).Scan(&plan)
	idea, _ := s.CreateBacklog(ctx, "ba1", pid, "feature", "planned", "")
	_, _ = db.Exec(ctx, `UPDATE tickets SET planned_sprint_id = $2, estimate = '3' WHERE id = $1`, idea.ID, plan)

	if err := bs.Archive(ctx, b.ID, "ba1"); err != nil {
		t.Fatal(err)
	}
	got, _ := bs.Get(ctx, b.ID, "ba1")
	if got.ArchivedAt == nil || listed() != 0 {
		t.Fatalf("archived: %+v, listed %d", got.ArchivedAt, listed())
	}
	var closed bool
	_ = db.QueryRow(ctx, `SELECT closed_at IS NOT NULL FROM sprints WHERE id = $1`, sprint).Scan(&closed)
	var nextSprints, plans int
	_ = db.QueryRow(ctx, `SELECT count(*) FROM sprints WHERE board_id = $1 AND closed_at IS NULL`, b.ID).Scan(&nextSprints)
	_ = db.QueryRow(ctx, `SELECT count(*) FROM planned_sprints WHERE board_id = $1`, b.ID).Scan(&plans)
	if !closed || nextSprints != 0 || plans != 0 {
		t.Fatalf("sprint closed %v, open sprints %d, plans %d", closed, nextSprints, plans)
	}
	if d, _ := s.Get(ctx, "ba1", idea.ID); d.PlannedSprintID != nil || d.Estimate != nil {
		t.Fatalf("planned ticket back to the backlog unplanned: %+v", d)
	}
	if d, _ := s.Get(ctx, "ba1", open.ID); d.BoardID == nil || *d.BoardID != b.ID || !d.Done {
		t.Fatal("finished work stays with the archived board")
	}
	// no new work on an archived board
	if _, err := s.Create(ctx, "ba1", b.Columns[0].ID, "task", "late", ""); !errors.Is(err, boards.ErrBoardArchived) {
		t.Fatalf("create on archived: %v", err)
	}
	if err := s.Move(ctx, "ba1", idea.ID, b.Columns[0].ID, 0); !errors.Is(err, boards.ErrBoardArchived) {
		t.Fatalf("move onto archived: %v", err)
	}
	if list, _ := bs.Archived(ctx, "ba1", pid); len(list) != 1 || list[0].ID != b.ID {
		t.Fatalf("archived list: %+v", list)
	}
	if list, _ := bs.Archived(ctx, "other", pid); len(list) != 0 {
		t.Fatal("another org saw archived boards")
	}
	// restored: back on the project and open for work
	if err := bs.Restore(ctx, b.ID, "ba1"); err != nil || listed() != 1 {
		t.Fatalf("restore: %v, listed %d", err, listed())
	}
	if _, err := s.Create(ctx, "ba1", b.Columns[0].ID, "task", "back", ""); err != nil {
		t.Fatalf("create after restore: %v", err)
	}
}
