package clerkhooks

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A deleted organization's data is kept 30 days, then purged for good: projects and everything under
// them, roadmaps, templates, notifications, keys, and its files queued for deletion on UploadThing.
func TestPurgeDeletedOrganizations(t *testing.T) {
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
	orgs := []string{"org_pg_old", "org_pg_new", "org_pg_live"}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := db.QueryRow(ctx, q, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}
	for _, org := range orgs {
		for _, q := range orgData {
			exec(q, org)
		}
		exec(`DELETE FROM deleted_files WHERE file_key = $1`, org+"-file")
		var pid, bid, tid string
		_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ($1, 'p') RETURNING id`, org).Scan(&pid)
		_ = db.QueryRow(ctx, `INSERT INTO boards (owner_clerk_id, project_id, name) VALUES ($1, $2, 'b') RETURNING id`, org, pid).Scan(&bid)
		_ = db.QueryRow(ctx, `INSERT INTO tickets (project_id, created_by, title, position) VALUES ($1, 'x', 't', 0) RETURNING id`, pid).Scan(&tid)
		exec(`INSERT INTO ticket_attachments (ticket_id, file_key, url, name, created_by) VALUES ($1, $2, 'https://x.ufs.sh/f/k', 'f', 'x')`, tid, org+"-file")
		exec(`INSERT INTO roadmaps (owner_clerk_id, project_id, title) VALUES ($1, $2, 'r')`, org, pid)
		exec(`INSERT INTO board_templates (org_id, name, style, estimate_scale, columns) VALUES ($1, 'tpl', 'kanban', 'none', '[]')`, org)
		exec(`INSERT INTO notifications (org_id, user_id, kind, text) VALUES ($1, 'u', 'mention', 'hi')`, org)
		exec(`INSERT INTO expired_attachments (org_id, ticket_id, ticket_key, ticket_title, name, size_bytes, created_by) VALUES ($1, gen_random_uuid(), 'K-1', 't', 'f', 1, 'u')`, org)
	}
	exec(`INSERT INTO deleted_orgs (org_id, deleted_at) VALUES ('org_pg_old', now() - interval '31 days'), ('org_pg_new', now() - interval '1 day')`)

	n, err := Purge(ctx, db, time.Now())
	if err != nil || n != 1 {
		t.Fatalf("purge: %d %v", n, err)
	}
	left := func(org string) int {
		return count(`SELECT (SELECT count(*) FROM projects WHERE owner_clerk_id = $1) + (SELECT count(*) FROM boards WHERE owner_clerk_id = $1)
			+ (SELECT count(*) FROM roadmaps WHERE owner_clerk_id = $1) + (SELECT count(*) FROM board_templates WHERE org_id = $1)
			+ (SELECT count(*) FROM notifications WHERE org_id = $1) + (SELECT count(*) FROM expired_attachments WHERE org_id = $1)
			+ (SELECT count(*) FROM ticket_attachments a JOIN tickets t ON t.id = a.ticket_id JOIN projects p ON p.id = t.project_id WHERE p.owner_clerk_id = $1)`, org)
	}
	if got := left("org_pg_old"); got != 0 {
		t.Fatalf("an org deleted 31 days ago still has %d rows", got)
	}
	if count(`SELECT count(*) FROM deleted_files WHERE file_key = 'org_pg_old-file'`) != 1 {
		t.Fatal("its file wasn't queued for deletion on UploadThing")
	}
	if count(`SELECT count(*) FROM deleted_orgs WHERE org_id = 'org_pg_old'`) != 0 {
		t.Fatal("purged org still listed")
	}
	if got := left("org_pg_new"); got != 7 {
		t.Fatalf("an org deleted yesterday should be kept (recoverable): %d rows", got)
	}
	if got := left("org_pg_live"); got != 7 {
		t.Fatalf("a live org was touched: %d rows", got)
	}
	if count(`SELECT count(*) FROM deleted_files WHERE file_key IN ('org_pg_new-file', 'org_pg_live-file')`) != 0 {
		t.Fatal("kept orgs' files queued for deletion")
	}
}
