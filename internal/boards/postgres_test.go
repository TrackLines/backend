package boards

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Needs a throwaway db: TEST_DATABASE_URL=postgres://... go test ./internal/boards/
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
	if _, err := db.Exec(ctx, `INSERT INTO users (clerk_id, email) VALUES ('b1', 'a@b.c'), ('b2', 'd@e.f') ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	s := Store{DB: db}

	// free tier: 5 concurrent creates with limit 1 → exactly one wins
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Create(ctx, "b1", "x", "", 1); err == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			} else if !errors.Is(err, ErrLimit) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("free-tier race: %d boards created", wins)
	}
	if _, err := s.Create(ctx, "b1", "y", "", -1); err != nil {
		t.Fatalf("paid unlimited: %v", err)
	}

	list, _ := s.ListByOwner(ctx, "b1")
	if len(list) != 2 {
		t.Fatalf("list: %d", len(list))
	}
	b := list[0]
	if _, err := s.Get(ctx, b.ID, "b2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("non-owner get: %v", err)
	}
	full, err := s.Get(ctx, b.ID, "b1")
	if err != nil || len(full.Columns) != 3 {
		t.Fatalf("get: %+v %v", full, err)
	}
	col := full.Columns[1].ID
	if _, err := db.Exec(ctx, `INSERT INTO tickets (board_id, column_id, title, position) VALUES ($1, $2, 't2', 1), ($1, $2, 't1', 0)`, b.ID, col); err != nil {
		t.Fatal(err)
	}
	full, _ = s.Get(ctx, b.ID, "b1")
	if ts := full.Columns[1].Tickets; len(ts) != 2 || ts[0].Title != "t1" {
		t.Fatalf("tickets: %+v", ts)
	}
	if err := s.Delete(ctx, b.ID, "b2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("non-owner delete: %v", err)
	}
	if err := s.Delete(ctx, b.ID, "b1"); err != nil {
		t.Fatal(err)
	}
}
