package tickets

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TL-175: a ticket says how many files are on it, so agents know to look at them before starting.
func TestAttachmentCount(t *testing.T) {
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
	if _, err := db.Exec(ctx, `DELETE FROM projects WHERE owner_clerk_id = 'ac1'`); err != nil {
		t.Fatal(err)
	}
	var pid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('ac1', 'Files') RETURNING id`).Scan(&pid)
	s := Store{DB: db}
	tk, _ := s.CreateBacklog(ctx, "ac1", pid, "task", "Build the card layout", "see the design")
	count := func() int {
		d, err := s.Get(ctx, "ac1", tk.ID)
		if err != nil {
			t.Fatal(err)
		}
		return d.Attachments
	}
	if n := count(); n != 0 {
		t.Fatalf("no files: %d", n)
	}
	for i, key := range []string{"k1", "k2"} {
		if _, err := db.Exec(ctx, `INSERT INTO ticket_attachments (ticket_id, file_key, url, name, size_bytes, content_type, created_by)
			VALUES ($1, $2, 'https://x.ufs.sh/f/'||$2, 'design.png', 10, 'image/png', 'user_a')`, tk.ID, key); err != nil {
			t.Fatalf("file %d: %v", i, err)
		}
	}
	if n := count(); n != 2 {
		t.Fatalf("two files: %d", n)
	}
}
