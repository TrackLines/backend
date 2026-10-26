package sprints

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tracklines/backend/internal/boards"
	"github.com/tracklines/backend/internal/tickets"
)

// TL-541: a board with imported tickets set aside can't start or plan a sprint until each is kept,
// closed as won't do, or put on a board; boards without any are unaffected.
func TestImportReview(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
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
	if _, err := db.Exec(ctx, `DELETE FROM boards WHERE owner_clerk_id = 'ir1'; DELETE FROM projects WHERE owner_clerk_id = 'ir1'`); err != nil {
		t.Fatal(err)
	}
	var pid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name, key_prefix) VALUES ('ir1', 'p', 'IR') RETURNING id`).Scan(&pid)
	bs, ts, s := boards.Store{DB: db}, tickets.Store{DB: db}, Store{DB: db}
	imported, _ := bs.Create(ctx, "ir1", pid, "imported", "")
	other, _ := bs.Create(ctx, "ir1", pid, "other", "")
	var set []string
	for _, title := range []string{"keep me", "close me", "move me"} {
		tk, _ := ts.CreateBacklog(ctx, "ir1", pid, "task", title, "")
		if _, err := db.Exec(ctx, `INSERT INTO import_reviews (ticket_id, board_id) VALUES ($1, $2)`, tk.ID, imported.ID); err != nil {
			t.Fatal(err)
		}
		set = append(set, tk.ID)
	}
	left := func() int {
		b, err := bs.Get(ctx, imported.ID, "ir1")
		if err != nil {
			t.Fatal(err)
		}
		return b.ToReview
	}
	if n := left(); n != 3 {
		t.Fatalf("to review: %d", n)
	}
	if list, err := s.Review(ctx, "ir1", imported.ID); err != nil || len(list) != 3 || list[0].Key == "" {
		t.Fatalf("review list: %+v %v", list, err)
	}
	if list, _ := s.Review(ctx, "other_org", imported.ID); len(list) != 0 {
		t.Fatal("another org sees the review list")
	}

	// refused until it's empty, starting or planning; the other board is fine
	if _, err := s.Start(ctx, "ir1", imported.ID, 14); !errors.Is(err, ErrNeedsRefining) {
		t.Fatalf("start: %v", err)
	}
	if _, err := s.Plan(ctx, "ir1", imported.ID, 14, ""); !errors.Is(err, ErrNeedsRefining) {
		t.Fatalf("plan: %v", err)
	}
	if _, err := s.Start(ctx, "ir1", other.ID, 14); err != nil {
		t.Fatalf("an unaffected board: %v", err)
	}

	// kept (only on its own board), closed as won't do, put on a board: each clears one
	if err := s.Keep(ctx, "ir1", other.ID, set[0]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("keep on another board: %v", err)
	}
	if err := s.Keep(ctx, "other_org", imported.ID, set[0]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("keep from another org: %v", err)
	}
	if err := s.Keep(ctx, "ir1", imported.ID, set[0]); err != nil || left() != 2 {
		t.Fatalf("keep: %v %d", err, left())
	}
	if err := ts.ResolveAsAgent(ctx, "ir1", "user", set[1], tickets.ResolutionWontDo, "", false); err != nil || left() != 1 {
		t.Fatalf("won't do: %v %d", err, left())
	}
	if _, err := s.Plan(ctx, "ir1", imported.ID, 14, ""); !errors.Is(err, ErrNeedsRefining) {
		t.Fatal("planned with one left")
	}
	if err := ts.Move(ctx, "ir1", set[2], imported.Columns[0].ID, 0); err != nil || left() != 0 {
		t.Fatalf("onto the board: %v %d", err, left())
	}
	if _, err := s.Start(ctx, "ir1", imported.ID, 14); err != nil {
		t.Fatalf("start once reviewed: %v", err)
	}
}
