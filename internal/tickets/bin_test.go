package tickets

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/boards"
)

// Deleting a ticket puts it in the bin (TL-256): gone from everywhere at once, but kept, comments and
// all, for 30 days; its files wait in today's deleted batch. The purge then removes it for good.
func TestTicketBin(t *testing.T) {
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
	if _, err := db.Exec(ctx, `DELETE FROM projects WHERE owner_clerk_id = 'bin1'`); err != nil {
		t.Fatal(err)
	}
	var pid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('bin1', 'Bin') RETURNING id`).Scan(&pid)
	b, _ := boards.Store{DB: db}.Create(ctx, "bin1", pid, "web", "")
	s := Store{DB: db}
	tk, _ := s.Create(ctx, "bin1", b.Columns[0].ID, "task", "binned", "")
	keep, _ := s.Create(ctx, "bin1", b.Columns[0].ID, "task", "kept", "")
	for _, q := range []string{`INSERT INTO ticket_comments (ticket_id, author, body) VALUES ($1, 'x', 'a comment')`,
		`INSERT INTO ticket_attachments (ticket_id, file_key, url, name, created_by) VALUES ($1, 'bin-file', 'https://x.ufs.sh/f/bin-file', 'f', 'x')`} {
		if _, err := db.Exec(ctx, q, tk.ID); err != nil {
			t.Fatal(err)
		}
	}
	_, _ = db.Exec(ctx, `DELETE FROM deleted_files WHERE file_key = 'bin-file'`)
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := db.QueryRow(ctx, q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	if err := s.Delete(ctx, "bin1", tk.ID); err != nil {
		t.Fatal(err)
	}
	// gone from everywhere that reads tickets
	if _, err := s.Get(ctx, "bin1", tk.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("binned ticket still readable: %v", err)
	}
	if err := s.Delete(ctx, "bin1", tk.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("binned twice: %v", err)
	}
	if n := count(`SELECT count(*) FROM tickets WHERE project_id = $1`, pid); n != 1 {
		t.Fatalf("live tickets: %d, want just the kept one", n)
	}
	// but kept, with its comments, and its file in today's deleted batch
	if n := count(`SELECT count(*) FROM tickets_all WHERE id = $1 AND deleted_at IS NOT NULL`, tk.ID); n != 1 {
		t.Fatal("binned ticket not kept")
	}
	if n := count(`SELECT count(*) FROM ticket_comments WHERE ticket_id = $1`, tk.ID); n != 1 {
		t.Fatal("its comments went with the delete")
	}
	if n := count(`SELECT count(*) FROM deleted_files WHERE file_key = 'bin-file' AND deleted_on = current_date`); n != 1 {
		t.Fatal("its file isn't in today's deleted batch")
	}

	// 29 days on it's still there; after 30 it's purged, comments and all; others untouched
	if n, err := PurgeBin(ctx, db, time.Now().Add(29*24*time.Hour)); err != nil || n != 0 {
		t.Fatalf("purged early: %d %v", n, err)
	}
	if n, err := PurgeBin(ctx, db, time.Now().Add(Retention+time.Hour)); err != nil || n < 1 {
		t.Fatalf("purge: %d %v", n, err)
	}
	if n := count(`SELECT count(*) FROM tickets_all WHERE id = $1`, tk.ID) + count(`SELECT count(*) FROM ticket_comments WHERE ticket_id = $1`, tk.ID); n != 0 {
		t.Fatal("purged ticket or its comments still there")
	}
	if _, err := s.Get(ctx, "bin1", keep.ID); err != nil {
		t.Fatalf("a live ticket was purged: %v", err)
	}
}

// The tickets view must show every column of tickets_all but deleted_at: a column added to the table
// and not the view would silently vanish from every query (TL-256).
func TestTicketsViewHasEveryColumn(t *testing.T) {
	ctx := context.Background()
	db, err := pgxpool.New(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var missing []string
	rows, err := db.Query(ctx, `SELECT column_name FROM information_schema.columns WHERE table_name = 'tickets_all' AND column_name <> 'deleted_at'
		EXCEPT SELECT column_name FROM information_schema.columns WHERE table_name = 'tickets'`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var c string
		_ = rows.Scan(&c)
		missing = append(missing, c)
	}
	if len(missing) > 0 {
		t.Fatalf("the tickets view is missing %v: recreate it in the migration that added them", missing)
	}
}
