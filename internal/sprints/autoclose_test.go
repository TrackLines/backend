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

// Needs a throwaway db: TEST_DATABASE_URL=postgres://... go test ./internal/sprints/
func TestAutoCloseSkipsFailures(t *testing.T) {
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
	if _, err := db.Exec(ctx, `DELETE FROM boards WHERE owner_clerk_id = 'ac1'; DELETE FROM projects WHERE owner_clerk_id = 'ac1'`); err != nil {
		t.Fatal(err)
	}
	var pid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('ac1', 'p') RETURNING id`).Scan(&pid)
	bs, s := boards.Store{DB: db}, Store{DB: db}
	poisoned, _ := bs.Create(ctx, "ac1", pid, "poisoned", "")
	healthy, _ := bs.Create(ctx, "ac1", pid, "healthy", "")

	// poisoned: sprint 1 is overdue, but number 2 is taken, so opening the next sprint fails. It's
	// the oldest overdue sprint, so it's tried first.
	var bad string
	if err := db.QueryRow(ctx, `INSERT INTO sprints (board_id, number, length_days, ends_at)
		VALUES ($1, 1, 7, now() - interval '2 days') RETURNING id`, poisoned.ID).Scan(&bad); err != nil {
		t.Fatal(err)
	}
	_, _ = db.Exec(ctx, `INSERT INTO sprints (board_id, number, length_days, ends_at, closed_at) VALUES ($1, 2, 7, now(), now())`, poisoned.ID)
	_, _ = db.Exec(ctx, `INSERT INTO sprints (board_id, number, length_days, ends_at) VALUES ($1, 1, 7, now() - interval '1 day')`, healthy.ID)

	n, err := s.AutoClose(ctx)
	if n < 1 || err == nil || !strings.Contains(err.Error(), bad) {
		t.Fatalf("closed %d, err %v", n, err)
	}
	var open int
	_ = db.QueryRow(ctx, `SELECT count(*) FROM sprints WHERE board_id = $1 AND closed_at IS NULL AND ends_at > now()`, healthy.ID).Scan(&open)
	if open != 1 {
		t.Fatal("the healthy board should have rolled into its next sprint")
	}
	_, _ = db.Exec(ctx, `DELETE FROM boards WHERE owner_clerk_id = 'ac1'`) // don't leave a failing sprint for other tests
}
