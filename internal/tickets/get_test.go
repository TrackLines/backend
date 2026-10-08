package tickets

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/boards"
)

// Needs a throwaway db: TEST_DATABASE_URL=postgres://... go test ./internal/tickets/
func TestGet(t *testing.T) {
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
	if _, err := db.Exec(ctx, `DELETE FROM users WHERE clerk_id IN ('g1', 'g2');
		DELETE FROM roadmaps WHERE owner_clerk_id IN ('g1', 'g2'); DELETE FROM boards WHERE owner_clerk_id IN ('g1', 'g2'); DELETE FROM projects WHERE owner_clerk_id IN ('g1', 'g2'); -- org-owned rows no longer cascade from users
		INSERT INTO users (clerk_id, email) VALUES ('g1', 'a@b.c'), ('g2', 'd@e.f')`); err != nil {
		t.Fatal(err)
	}
	var pid string
	if err := db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('g1', 'Proj') RETURNING id`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	b, _ := boards.Store{DB: db}.Create(ctx, "g1", pid, "Team", "")
	s := Store{DB: db}
	onBoard, err := s.Create(ctx, "g1", b.Columns[1].ID, "bug", "on board", "")
	if err != nil {
		t.Fatal(err)
	}
	inBacklog, _ := s.CreateBacklog(ctx, "g1", pid, "feature", "in backlog", "")

	d, err := s.Get(ctx, "g1", onBoard.ID)
	if err != nil || d.Title != "on board" || d.ProjectName != "Proj" || d.BoardName == nil || *d.BoardName != "Team" ||
		d.ColumnName == nil || *d.ColumnName != "In progress" || d.Type != "bug" {
		t.Fatalf("board ticket: %+v %v", d, err)
	}
	d, err = s.Get(ctx, "g1", inBacklog.ID)
	if err != nil || d.BoardID != nil || d.ColumnName != nil || d.ProjectName != "Proj" {
		t.Fatalf("backlog ticket: %+v %v", d, err)
	}
	if _, err := s.Get(ctx, "g2", onBoard.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("non-owner: %v", err)
	}
	if _, err := s.Get(ctx, "g1", "not-a-uuid"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bad id: %v", err)
	}
}

func TestGetDone(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	db, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	TestGet(t) // migrations + users
	var pid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('g1', 'Proj2') RETURNING id`).Scan(&pid)
	b, _ := boards.Store{DB: db}.Create(ctx, "g1", pid, "Team", "")
	s := Store{DB: db}
	open, _ := s.Create(ctx, "g1", b.Columns[0].ID, "task", "open", "")
	closed, _ := s.Create(ctx, "g1", b.Columns[2].ID, "task", "closed", "")
	backlogged, _ := s.CreateBacklog(ctx, "g1", pid, "task", "backlog", "")
	for _, c := range []struct {
		id   string
		want bool
	}{{open.ID, false}, {closed.ID, true}, {backlogged.ID, false}} {
		if d, err := s.Get(ctx, "g1", c.id); err != nil || d.Done != c.want {
			t.Fatalf("%s: done=%v want %v (%v)", c.id, d != nil && d.Done, c.want, err)
		}
	}
}
