package retention

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tracklines/backend/inbox"
)

func testDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	url := os.Getenv("TEST_DATABASE_URL")
	m, err := migrate.New("file://../internal/migrations", "pgx5"+url[len("postgres"):])
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
	t.Cleanup(db.Close)
	return db
}

// The nightly purge the automation service runs (TL-431): the bin, a deleted organization, a year-old
// attachment (its warning in the inbox) and an expired link code, each counted; what's still within
// its 30 days stays.
func TestPurge(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	now := time.Now()
	day := 24 * time.Hour
	const org, gone = "org_retp", "org_retp_gone"
	for _, q := range []string{
		`DELETE FROM projects WHERE owner_clerk_id IN ('org_retp', 'org_retp_gone')`, `DELETE FROM org_admins WHERE org_id = 'org_retp'`,
		`DELETE FROM notifications WHERE org_id = 'org_retp'`, `DELETE FROM deleted_orgs WHERE org_id IN ('org_retp', 'org_retp_gone')`,
		`DELETE FROM chat_link_codes WHERE code_hash LIKE 'retp-%'`,
		`INSERT INTO org_admins (org_id, user_clerk_id, granted_by) VALUES ('org_retp', 'user_retp_admin', 'test')`,
	} {
		if _, err := db.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	var pid, binned, recent, kept string
	db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ($1, 'Retention') RETURNING id`, org).Scan(&pid)
	db.QueryRow(ctx, `INSERT INTO tickets (project_id, created_by, title, position) VALUES ($1, 'x', 'old bin', 0) RETURNING id`, pid).Scan(&binned)
	db.QueryRow(ctx, `INSERT INTO tickets (project_id, created_by, title, position) VALUES ($1, 'x', 'new bin', 0) RETURNING id`, pid).Scan(&recent)
	db.QueryRow(ctx, `INSERT INTO tickets (project_id, created_by, title, position) VALUES ($1, 'x', 'with a file', 0) RETURNING id`, pid).Scan(&kept)
	db.Exec(ctx, `UPDATE tickets_all SET deleted_at = $2 WHERE id = $1`, binned, now.Add(-31*day))
	db.Exec(ctx, `UPDATE tickets_all SET deleted_at = $2 WHERE id = $1`, recent, now.Add(-29*day))
	if _, err := db.Exec(ctx, `INSERT INTO ticket_attachments (ticket_id, file_key, url, name, created_by, created_at)
		VALUES ($1, 'retp-file', 'https://x.ufs.sh/f/retp-file', 'plan.pdf', 'user_retp_up', $2)`, kept, now.Add(-366*day)); err != nil {
		t.Fatal(err)
	}
	var gonePID string
	db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ($1, 'Gone') RETURNING id`, gone).Scan(&gonePID)
	db.Exec(ctx, `INSERT INTO deleted_orgs (org_id, deleted_at) VALUES ($1, $2)`, gone, now.Add(-31*day))

	r, err := Purge(ctx, db, ToInbox(db), now)
	if err != nil {
		t.Fatal(err)
	}
	if r.Tickets < 1 || r.Orgs < 1 || r.Warned != 1 {
		t.Fatalf("purge: %+v", r)
	}
	count := func(q string, args ...any) (n int) {
		db.QueryRow(ctx, q, args...).Scan(&n)
		return
	}
	if count(`SELECT count(*) FROM tickets_all WHERE id = $1`, binned) != 0 || count(`SELECT count(*) FROM tickets_all WHERE id = $1`, recent) != 1 {
		t.Fatal("the bin: purged the wrong tickets")
	}
	if count(`SELECT count(*) FROM projects WHERE owner_clerk_id = $1`, gone) != 0 {
		t.Fatal("the deleted organization's project is still there")
	}
	// the warning reached the inbox of the admin and the uploader, from TrackLin.es
	var text string
	db.QueryRow(ctx, `SELECT text FROM notifications WHERE org_id = $1 AND kind = 'retention' AND user_id = 'user_retp_up'`, org).Scan(&text)
	if !strings.Contains(text, "will delete a year-old file") || count(`SELECT count(*) FROM notifications WHERE org_id = $1 AND user_id = 'user_retp_admin'`, org) != 1 {
		t.Fatalf("warning not in the inbox: %q", text)
	}
	if r.String() == "" || !strings.Contains(r.String(), "ticket(s) from the bin") {
		t.Fatalf("summary: %q", r)
	}

	// a notice that fails to send is reported, and the file is still marked as warned
	db.Exec(ctx, `INSERT INTO ticket_attachments (ticket_id, file_key, url, name, created_by, created_at)
		VALUES ($1, 'retp-file2', 'https://x.ufs.sh/f/retp-file2', 'b.pdf', 'user_retp_up', $2)`, kept, now.Add(-366*day))
	failing := func(context.Context, string, []string, inbox.Notice) error { return errors.New("inbox down") }
	if n, err := WarnOld(ctx, db, failing, now); n != 1 || err == nil || !strings.Contains(err.Error(), "inbox down") {
		t.Fatalf("a failed notice: %d %v", n, err)
	}
	if count(`SELECT count(*) FROM ticket_attachments WHERE file_key = 'retp-file2' AND warned_at IS NOT NULL`) != 1 {
		t.Fatal("the file wasn't marked as warned")
	}
}

// People only, each once, never the one who did it.
func TestInboxAdd(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	db.Exec(ctx, `DELETE FROM notifications WHERE org_id = 'org_inbox'`)
	err := inbox.Add(ctx, db, "org_inbox", []string{"user_a", "user_a", "Claude", "user_me", "", "user_b"},
		inbox.Notice{Kind: "assigned", Text: "assigned TL-1 to you", Actor: "user_me"})
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := db.Query(ctx, `SELECT user_id FROM notifications WHERE org_id = 'org_inbox' ORDER BY user_id`)
	var got []string
	for rows.Next() {
		var u string
		rows.Scan(&u)
		got = append(got, u)
	}
	if strings.Join(got, ",") != "user_a,user_b" {
		t.Fatalf("inbox: %v", got)
	}
	if err := inbox.Add(ctx, db, "org_inbox", []string{"user_me"}, inbox.Notice{Actor: "user_me"}); err != nil {
		t.Fatal("nobody to tell is not an error")
	}
}
