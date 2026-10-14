package integrations

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/tickets"
)

// A PR links to tickets it mentions by key, by ticket link, or by one of our short links (TL-283);
// only the linked project's tickets, and only short links Tracklines made.
func TestPRMentionsByLink(t *testing.T) {
	ctx := context.Background()
	dbURL := os.Getenv("TEST_DATABASE_URL")
	m, err := migrate.New("file://../migrations", "pgx5"+dbURL[len("postgres"):])
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatal(err)
	}
	db, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(ctx, `DELETE FROM projects WHERE owner_clerk_id IN ('org_ml', 'org_ml2'); DELETE FROM short_links WHERE short LIKE 'ml%'`); err != nil {
		t.Fatal(err)
	}
	var pid, otherPid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name, key_prefix) VALUES ('org_ml', 'Short', 'ONE') RETURNING id`).Scan(&pid)
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name, key_prefix) VALUES ('org_ml2', 'Theirs', 'ONE') RETURNING id`).Scan(&otherPid)
	ts := tickets.Store{DB: db}
	byKey, _ := ts.CreateBacklog(ctx, "org_ml", pid, "task", "by key", "")
	byLink, _ := ts.CreateBacklog(ctx, "org_ml", pid, "task", "by link", "")
	byShort, _ := ts.CreateBacklog(ctx, "org_ml", pid, "task", "by short link", "")
	theirs, _ := ts.CreateBacklog(ctx, "org_ml2", otherPid, "task", "another org's", "")
	if _, err := db.Exec(ctx, `INSERT INTO project_repos (project_id, org_id, provider, full_name) VALUES ($1, 'org_ml', 'github', '1tn-pw/short-service')`, pid); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO short_links (url, short) VALUES ($1, 'mlAbC1'), ($2, 'mlXyZ9')`,
		"https://tracklin.es/tickets/"+byShort.ID, "https://tracklin.es/tickets/"+theirs.ID); err != nil {
		t.Fatal(err)
	}
	h := System{store: Store{DB: db}, tickets: ts}.WithShortLinks("https://1tn.pw")
	linked := func(ticket string) bool {
		var n int
		_ = db.QueryRow(ctx, `SELECT count(*) FROM ticket_prs WHERE ticket_id = $1`, ticket).Scan(&n)
		return n > 0
	}
	pr := func(number int, title, body string) {
		t.Helper()
		if err := h.linkPR(ctx, []string{"org_ml"}, PullRequest{Provider: "github", Repo: "1tn-pw/short-service", Number: number,
			URL: "https://github.com/1tn-pw/short-service/pull/1", Title: title, State: "open", Branch: "railway", body: body}); err != nil {
			t.Fatal(err)
		}
	}
	pr(1, "Fix ONE-1", "")
	pr(2, "Tester", "See https://tracklin.es/tickets/"+byLink.ID+" for details")
	pr(3, "Tester", "Mentioned https://1tn.pw/mlAbC1")
	// not ours to link: another org's ticket by link or short link, an unknown short code, a short link on another host
	pr(4, "Tester", "https://tracklin.es/tickets/"+theirs.ID+" https://1tn.pw/mlXyZ9 https://1tn.pw/mlNope0 https://bit.ly/mlAbC1")
	for name, c := range map[string]struct {
		id   string
		want bool
	}{"key": {byKey.ID, true}, "ticket link": {byLink.ID, true}, "short link": {byShort.ID, true}, "another org's": {theirs.ID, false}} {
		if got := linked(c.id); got != c.want {
			t.Errorf("%s: linked %v, want %v", name, got, c.want)
		}
	}
	var onShort int
	_ = db.QueryRow(ctx, `SELECT count(*) FROM ticket_prs WHERE ticket_id = $1 AND number = 4`, byShort.ID).Scan(&onShort)
	if onShort != 0 {
		t.Fatal("a short link on another host matched")
	}
}
