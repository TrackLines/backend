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
func TestDependencies(t *testing.T) {
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
	if _, err := db.Exec(ctx, `DELETE FROM users WHERE clerk_id IN ('dp1', 'dp2');
		DELETE FROM roadmaps WHERE owner_clerk_id IN ('dp1', 'dp2'); DELETE FROM boards WHERE owner_clerk_id IN ('dp1', 'dp2'); DELETE FROM projects WHERE owner_clerk_id IN ('dp1', 'dp2'); -- org-owned rows no longer cascade from users
		INSERT INTO users (clerk_id, email) VALUES ('dp1', 'a@b.c'), ('dp2', 'd@e.f')`); err != nil {
		t.Fatal(err)
	}
	var pid, otherPID string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('dp1', 'p') RETURNING id`).Scan(&pid)
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('dp1', 'other') RETURNING id`).Scan(&otherPID)
	b, _ := boards.Store{DB: db}.Create(ctx, "dp1", pid, "Team", "")
	todo, done := b.Columns[0].ID, b.Columns[2].ID
	s := Store{DB: db}
	mk := func(title string) string {
		tk, err := s.Create(ctx, "dp1", todo, "task", title, "")
		if err != nil {
			t.Fatal(err)
		}
		return tk.ID
	}
	a, bb, c := mk("A schema"), mk("B api"), mk("C ui")
	elsewhere, _ := s.CreateBacklog(ctx, "dp1", otherPID, "task", "other project", "")

	// C needs A and B
	if err := s.SetBlockedBy(ctx, "dp1", c, []string{a, bb, a}); err != nil {
		t.Fatal(err)
	}
	d, _ := s.Get(ctx, "dp1", c)
	if !d.Blocked || len(d.BlockedBy) != 2 {
		t.Fatalf("C should be blocked by A and B: %+v", d)
	}
	if da, _ := s.Get(ctx, "dp1", a); len(da.Blocks) != 1 || da.Blocks[0].ID != c || da.Blocked {
		t.Fatalf("A blocks C: %+v", da)
	}
	if _, err := s.Claim(ctx, "dp1", "claude", c); !errors.Is(err, ErrBlocked) {
		t.Fatalf("claiming a blocked ticket: %v", err)
	}
	// board view carries the flag too
	if full, _ := (boards.Store{DB: db}).Get(ctx, b.ID, "dp1"); !full.Columns[0].Tickets[2].Blocked && !full.Columns[0].Tickets[0].Blocked && !full.Columns[0].Tickets[1].Blocked {
		t.Fatal("board tickets should report blocked")
	}

	// finishing A and B unblocks C
	for _, id := range []string{a, bb} {
		if err := s.Move(ctx, "dp1", id, done, 0); err != nil {
			t.Fatal(err)
		}
	}
	if d, _ := s.Get(ctx, "dp1", c); d.Blocked || !d.BlockedBy[0].Done {
		t.Fatalf("C should be unblocked once A and B are done: %+v", d)
	}
	if _, err := s.Claim(ctx, "dp1", "claude", c); err != nil {
		t.Fatalf("claim after unblock: %v", err)
	}

	// invalid links
	if err := s.SetBlockedBy(ctx, "dp1", a, []string{a}); !errors.Is(err, ErrSelfBlock) {
		t.Fatalf("self: %v", err)
	}
	if err := s.SetBlockedBy(ctx, "dp1", a, []string{elsewhere.ID}); !errors.Is(err, ErrOtherProject) {
		t.Fatalf("other project: %v", err)
	}
	if err := s.SetBlockedBy(ctx, "dp1", a, []string{c}); !errors.Is(err, ErrCycle) {
		t.Fatalf("2-cycle (C waits on A, A waits on C): %v", err)
	}
	dd := mk("D")
	_ = s.SetBlockedBy(ctx, "dp1", dd, []string{c}) // D waits on C waits on A
	if err := s.SetBlockedBy(ctx, "dp1", a, []string{dd}); !errors.Is(err, ErrCycle) {
		t.Fatalf("3-cycle: %v", err)
	}
	if err := s.SetBlockedBy(ctx, "dp2", c, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("non-owner: %v", err)
	}

	// clearing and cascading
	if err := s.SetBlockedBy(ctx, "dp1", c, []string{}); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.Get(ctx, "dp1", c); len(d.BlockedBy) != 0 {
		t.Fatalf("cleared: %+v", d)
	}
	_ = s.SetBlockedBy(ctx, "dp1", dd, []string{c})
	if err := s.Delete(ctx, "dp1", c); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.Get(ctx, "dp1", dd); d.Blocked || len(d.BlockedBy) != 0 {
		t.Fatalf("deleting the blocker should free D: %+v", d)
	}
}
