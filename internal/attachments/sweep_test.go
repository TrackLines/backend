package attachments

import (
	"context"
	"errors"
	"os"
	"slices"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
)

type fakeFiles struct {
	deleted []string
	fail    bool
}

func (f *fakeFiles) DeleteFiles(_ context.Context, keys ...string) error {
	if f.fail {
		return errors.New("uploadthing down")
	}
	f.deleted = append(f.deleted, keys...)
	return nil
}

// However an attachment goes (here: its whole ticket is deleted), its file is queued and the sweep
// deletes it from UploadThing; if UploadThing fails, it stays queued for the next sweep (TL-206).
func TestDeletedTicketsFilesAreSwept(t *testing.T) {
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
	if _, err := db.Exec(ctx, `DELETE FROM projects WHERE owner_clerk_id = 'org_sw'; DELETE FROM deleted_files`); err != nil {
		t.Fatal(err)
	}
	var pid, tid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('org_sw', 'p') RETURNING id`).Scan(&pid)
	_ = db.QueryRow(ctx, `INSERT INTO tickets (project_id, created_by, title, position) VALUES ($1, 'x', 't', 0) RETURNING id`, pid).Scan(&tid)
	if _, err := db.Exec(ctx, `INSERT INTO ticket_attachments (ticket_id, file_key, url, name, created_by) VALUES
		($1, 'sw-a', 'https://x.ufs.sh/f/sw-a', 'a', 'x'), ($1, 'sw-b', 'https://x.ufs.sh/f/sw-b', 'b', 'x')`, tid); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `DELETE FROM tickets WHERE id = $1`, tid); err != nil {
		t.Fatal(err)
	}

	files := &fakeFiles{fail: true}
	s := Sweeper{DB: db, Files: files}
	if _, err := s.Sweep(ctx); err == nil {
		t.Fatal("a failing UploadThing should be reported")
	}
	var queued, attempts int
	_ = db.QueryRow(ctx, `SELECT count(*), coalesce(max(attempts), 0) FROM deleted_files WHERE file_key IN ('sw-a', 'sw-b')`).Scan(&queued, &attempts)
	if queued != 2 || attempts != 1 {
		t.Fatalf("after a failed sweep: %d queued, %d attempts", queued, attempts)
	}
	files.fail = false
	if n, err := s.Sweep(ctx); err != nil || n != 2 {
		t.Fatalf("sweep: %d %v", n, err)
	}
	slices.Sort(files.deleted)
	if !slices.Equal(files.deleted, []string{"sw-a", "sw-b"}) {
		t.Fatalf("deleted on UploadThing: %v", files.deleted)
	}
	_ = db.QueryRow(ctx, `SELECT count(*) FROM deleted_files`).Scan(&queued)
	if queued != 0 {
		t.Fatalf("still queued: %d", queued)
	}
	if n, _ := s.Sweep(ctx); n != 0 {
		t.Fatalf("nothing left to sweep: %d", n)
	}
}
