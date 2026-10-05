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
func TestStore(t *testing.T) {
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
	if _, err := db.Exec(ctx, `INSERT INTO users (clerk_id, email) VALUES ('t1', 'a@b.c'), ('t2', 'd@e.f') ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	var pid string
	if err := db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('t1', 'p') RETURNING id`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	b, err := boards.Store{DB: db}.Create(ctx, "t1", pid, "b", "")
	if err != nil {
		t.Fatal(err)
	}
	todo, doing := b.Columns[0].ID, b.Columns[1].ID
	s := Store{DB: db}

	if _, err := s.Create(ctx, "t2", todo, "nope", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("non-owner create: %v", err)
	}
	var ids []string
	for _, title := range []string{"a", "b", "c"} {
		tk, err := s.Create(ctx, "t1", todo, title, "")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, tk.ID)
	}
	order := func(col string) (out []string) {
		full, _ := boards.Store{DB: db}.Get(ctx, b.ID, "t1")
		for _, c := range full.Columns {
			if c.ID == col {
				for i, tk := range c.Tickets {
					if tk.Position != i {
						t.Fatalf("gap in positions: %+v", c.Tickets)
					}
					out = append(out, tk.Title)
				}
			}
		}
		return
	}
	if err := s.Move(ctx, "t1", ids[2], todo, 0); err != nil { // reorder: c to top
		t.Fatal(err)
	}
	if got := order(todo); len(got) != 3 || got[0] != "c" || got[1] != "a" {
		t.Fatalf("reorder: %v", got)
	}
	if err := s.Move(ctx, "t1", ids[0], doing, 99); err != nil { // a → doing, clamped
		t.Fatal(err)
	}
	if got := order(todo); len(got) != 2 || got[0] != "c" || got[1] != "b" {
		t.Fatalf("source after move: %v", got)
	}
	if got := order(doing); len(got) != 1 || got[0] != "a" {
		t.Fatalf("target after move: %v", got)
	}
	if err := s.Move(ctx, "t2", ids[1], doing, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("non-owner move: %v", err)
	}
	other, _ := boards.Store{DB: db}.Create(ctx, "t1", pid, "other", "")
	if err := s.Move(ctx, "t1", ids[1], other.Columns[0].ID, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-board move: %v", err)
	}
	if err := s.Update(ctx, "t2", ids[1], "x", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("non-owner update: %v", err)
	}
	if err := s.Update(ctx, "t1", ids[1], "bee", "d"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "t1", ids[1]); err != nil {
		t.Fatal(err)
	}
}
