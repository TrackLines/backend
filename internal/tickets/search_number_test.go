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

// TL-140: "139" finds TL-139 (its number, any project), ahead of tickets that only mention 139.
func TestSearchByNumber(t *testing.T) {
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
	if _, err := db.Exec(ctx, `DELETE FROM boards WHERE owner_clerk_id = 'num1'; DELETE FROM projects WHERE owner_clerk_id = 'num1'`); err != nil {
		t.Fatal(err)
	}
	var pid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name, key_prefix) VALUES ('num1', 'Numbers', 'NUM') RETURNING id`).Scan(&pid)
	b, _ := boards.Store{DB: db}.Create(ctx, "num1", pid, "web", "")
	s := Store{DB: db}
	mentions, _ := s.Create(ctx, "num1", b.Columns[0].ID, "task", "Mentions it", "follow-up to 2")
	second, _ := s.Create(ctx, "num1", b.Columns[0].ID, "task", "The second ticket", "")
	if second.Key != "NUM-2" {
		t.Fatalf("setup: key %q", second.Key)
	}

	got, err := s.Search(ctx, "num1", SearchQuery{Text: "2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != second.ID || got[1].ID != mentions.ID {
		t.Fatalf("number first, then mentions: %+v", got)
	}
	if got, _ := s.Search(ctx, "num1", SearchQuery{Text: "99"}); len(got) != 0 {
		t.Fatalf("no such number: %+v", got)
	}
	// words still search as before
	if got, _ := s.Search(ctx, "num1", SearchQuery{Text: "seco"}); len(got) != 1 || got[0].ID != second.ID {
		t.Fatalf("prefix words: %+v", got)
	}
}
