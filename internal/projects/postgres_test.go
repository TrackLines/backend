package projects

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
	"github.com/tracklines/backend/internal/boards"
	"github.com/tracklines/backend/internal/roadmaps"
)

// Needs a throwaway db: TEST_DATABASE_URL=postgres://... go test ./internal/projects/
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
	if _, err := db.Exec(ctx, `DELETE FROM users WHERE clerk_id IN ('pr1', 'pr2');
		INSERT INTO users (clerk_id, email) VALUES ('pr1', 'a@b.c'), ('pr2', 'd@e.f')`); err != nil {
		t.Fatal(err)
	}
	s := Store{DB: db}

	// free tier: 5 concurrent creates with limit 1 → exactly one project
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Create(ctx, "pr1", "x", "", 1); err == nil {
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
		t.Fatalf("free-tier race: %d projects created", wins)
	}
	if _, err := s.Create(ctx, "pr1", "second", "", -1); err != nil {
		t.Fatalf("paid unlimited: %v", err)
	}
	list, _ := s.List(ctx, "pr1")
	if len(list) != 2 {
		t.Fatalf("list: %+v", list)
	}
	p := list[0]

	// unlimited boards inside the free project, plus a roadmap
	for _, team := range []string{"Backend", "Frontend", "Design"} {
		if _, err := (boards.Store{DB: db}).Create(ctx, "pr1", p.ID, team, ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := (roadmaps.Store{DB: db}).Create(ctx, "pr1", p.ID, "Q1", "", roadmaps.Public); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, p.ID, "pr1")
	if err != nil || len(got.Boards) != 3 || len(got.Roadmaps) != 1 {
		t.Fatalf("get: %+v %v", got, err)
	}
	if _, err := s.Get(ctx, p.ID, "pr2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("non-owner get: %v", err)
	}
	if err := s.Update(ctx, p.ID, "pr2", "hijack", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("non-owner update: %v", err)
	}
	if err := s.Update(ctx, p.ID, "pr1", "Renamed", "d"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "not-a-uuid", "pr1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bad uuid: %v", err)
	}
	if err := s.Delete(ctx, p.ID, "pr1"); err != nil {
		t.Fatal(err)
	}
	var left int
	_ = db.QueryRow(ctx, `SELECT count(*) FROM boards WHERE project_id = $1`, p.ID).Scan(&left)
	if left != 0 {
		t.Fatalf("delete should cascade to boards, %d left", left)
	}
}
