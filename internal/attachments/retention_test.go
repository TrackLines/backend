package attachments

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/notifications"
)

type told struct {
	to []string
	e  notifications.Event
}
type fakeNotifier struct{ got []told }

func (f *fakeNotifier) Notify(_ context.Context, _ string, to []string, e notifications.Event) error {
	f.got = append(f.got, told{to, e})
	return nil
}

// Year-old attachments are warned about (admins and uploader told), kept on request for another year,
// and otherwise deleted at 13 months, never sooner than 28 days after the warning (TL-259).
func TestAttachmentRetention(t *testing.T) {
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
	const org = "org_ret"
	for _, q := range []string{`DELETE FROM projects WHERE owner_clerk_id = 'org_ret'`, `DELETE FROM org_admins WHERE org_id = 'org_ret'`, `DELETE FROM expired_attachments WHERE org_id = 'org_ret'`,
		`INSERT INTO org_admins (org_id, user_clerk_id, granted_by) VALUES ('org_ret', 'user_admin', 'test')`} {
		if _, err := db.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	var pid, tid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ($1, 'Old') RETURNING id`, org).Scan(&pid)
	_ = db.QueryRow(ctx, `INSERT INTO tickets (project_id, created_by, title, position) VALUES ($1, 'x', 'old work', 0) RETURNING id`, pid).Scan(&tid)
	now := time.Now()
	add := func(key, by string, age time.Duration) string {
		t.Helper()
		var id string
		if err := db.QueryRow(ctx, `INSERT INTO ticket_attachments (ticket_id, file_key, url, name, created_by, created_at)
			VALUES ($1, $2, 'https://x.ufs.sh/f/'||$2, $2, $3, $4) RETURNING id`, tid, key, by, now.Add(-age)).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	day := 24 * time.Hour
	old := add("ret-old", "user_up", 366*day)
	ancient := add("ret-ancient", "user_up", 800*day)
	fresh := add("ret-fresh", "user_up", 30*day)
	exists := func(id string) bool {
		var n int
		_ = db.QueryRow(ctx, `SELECT count(*) FROM ticket_attachments WHERE id = $1`, id).Scan(&n)
		return n == 1
	}

	// a year on: warned about once, admins and the uploader told in one notification for the ticket
	n := &fakeNotifier{}
	if got, err := WarnOld(ctx, db, n, now); err != nil || got != 2 {
		t.Fatalf("warned %d (%v), want the two year-old files", got, err)
	}
	if len(n.got) != 1 || !slices.Contains(n.got[0].to, "user_admin") || !slices.Contains(n.got[0].to, "user_up") ||
		n.got[0].e.TicketID != tid || !strings.Contains(n.got[0].e.Text, "2 year-old files") || !strings.Contains(n.got[0].e.Text, "old work") {
		t.Fatalf("notification: %+v", n.got)
	}
	if got, _ := WarnOld(ctx, db, n, now.Add(day)); got != 0 {
		t.Fatalf("warned again: %d", got)
	}
	// the ticket's file list says when each warned file goes
	list, _ := Store{DB: db}.List(ctx, org, tid)
	for _, a := range list {
		if (a.ID == fresh) != (a.DeleteOn == nil) {
			t.Fatalf("delete_on: %+v", a)
		}
	}

	// not before 13 months, nor before 28 days' notice; then deleted, into today's deleted batch
	if got, _ := DeleteExpired(ctx, db, now.Add(20*day)); got != 0 {
		t.Fatalf("deleted early: %d", got)
	}
	if got, _ := DeleteExpired(ctx, db, now.Add(40*day)); got != 2 || exists(old) || exists(ancient) || !exists(fresh) {
		t.Fatalf("deleted %d; old=%v ancient=%v fresh=%v", got, exists(old), exists(ancient), exists(fresh))
	}
	var queued int
	_ = db.QueryRow(ctx, `SELECT count(*) FROM deleted_files WHERE file_key IN ('ret-old', 'ret-ancient') AND deleted_on = current_date`).Scan(&queued)
	if queued != 2 {
		t.Fatalf("files queued for deletion: %d", queued)
	}

	// keeping: the uploader or an admin, not another member, not an API key; it starts the year again
	kept := add("ret-kept", "user_up", 366*day)
	if _, err := WarnOld(ctx, db, n, now); err != nil {
		t.Fatal(err)
	}
	h := System{store: Store{DB: db}, IsAdmin: func(_ context.Context, _, user string) (bool, error) { return user == "user_admin", nil }}
	keep := func(c context.Context) int {
		req := httptest.NewRequest("POST", "/", nil).WithContext(c)
		req.SetPathValue("id", kept)
		rec := httptest.NewRecorder()
		h.Keep(rec, req)
		return rec.Code
	}
	session := func(user string) context.Context {
		c := &clerk.SessionClaims{RegisteredClaims: clerk.RegisteredClaims{Subject: user}}
		c.ActiveOrganizationID = org
		return clerk.ContextWithSessionClaims(ctx, c)
	}
	if code := keep(session("user_someone")); code != 403 {
		t.Fatalf("another member kept it: %d", code)
	}
	if code := keep(auth.WithAPIKey(ctx, "user_up", org, "agent", "")); code != 403 {
		t.Fatalf("an API key kept it: %d", code)
	}
	if code := keep(session("user_up")); code != 204 {
		t.Fatalf("uploader keep: %d", code)
	}
	if code := keep(session("user_admin")); code != 204 {
		t.Fatalf("admin keep: %d", code)
	}
	if got, _ := DeleteExpired(ctx, db, now.Add(60*day)); got != 0 || !exists(kept) {
		t.Fatal("a kept file was deleted")
	}
	if got, _ := WarnOld(ctx, db, n, now.Add(300*day)); got != 0 {
		t.Fatal("a kept file was warned about again within its year")
	}

	// the clean-up page (TL-260): admins see what's due and what went; nobody else does
	due := add("ret-due", "user_up", 370*day)
	if _, err := WarnOld(ctx, db, n, now); err != nil {
		t.Fatal(err)
	}
	report := func(c context.Context) (int, Retention) {
		rec := httptest.NewRecorder()
		h.Retention(rec, httptest.NewRequest("GET", "/", nil).WithContext(c))
		var out Retention
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	code, got := report(session("user_admin"))
	if code != 200 || len(got.Due) != 1 || got.Due[0].ID != due || got.Due[0].TicketID != tid || got.Due[0].CreatedBy != "user_up" {
		t.Fatalf("due: %d %+v", code, got.Due)
	}
	names := []string{}
	for _, f := range got.Deleted {
		names = append(names, f.Name)
		if f.TicketID != tid || f.TicketTitle != "old work" || f.TicketKey == "" {
			t.Fatalf("deleted entry: %+v", f)
		}
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"ret-ancient", "ret-old"}) {
		t.Fatalf("deleted: %v", names)
	}
	for who, c := range map[string]context.Context{
		"the uploader": session("user_up"), "an API key": auth.WithAPIKey(ctx, "user_admin", org, "agent", ""),
	} {
		if code, _ := report(c); code != 403 {
			t.Fatalf("%s saw the clean-up page: %d", who, code)
		}
	}
	other := &clerk.SessionClaims{RegisteredClaims: clerk.RegisteredClaims{Subject: "user_admin"}}
	other.ActiveOrganizationID = "org_ret_other"
	h.IsAdmin = func(context.Context, string, string) (bool, error) { return true, nil }
	if code, got := report(clerk.ContextWithSessionClaims(ctx, other)); code != 200 || len(got.Due)+len(got.Deleted) != 0 {
		t.Fatalf("another org's admin saw: %d %+v", code, got)
	}
}
