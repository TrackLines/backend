package integrations

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/organizations"
)

func allOn(string) bool { return true }

// Bitbucket ships behind its flags.gg flag (TL-205): while it's off, every Bitbucket route answers 404
// as if it didn't exist, existing Bitbucket links are left out of a project's repos, and nothing can
// be linked to it. GitHub isn't flagged.
func TestBitbucketBehindItsFlag(t *testing.T) {
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
	if _, err := db.Exec(ctx, `DELETE FROM projects WHERE owner_clerk_id = 'org_flag'`); err != nil {
		t.Fatal(err)
	}
	var pid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('org_flag', 'p') RETURNING id`).Scan(&pid)
	if _, err := db.Exec(ctx, `INSERT INTO project_repos (project_id, org_id, provider, full_name) VALUES ($1, 'org_flag', 'github', 'acme/web'), ($1, 'org_flag', 'bitbucket', 'acme/old')`, pid); err != nil {
		t.Fatal(err)
	}
	var bbLink string
	_ = db.QueryRow(ctx, `SELECT id FROM project_repos WHERE org_id = 'org_flag' AND provider = 'bitbucket'`).Scan(&bbLink)

	member := auth.WithAPIKey(ctx, "user_flag", "org_flag", "ui", "")
	off := System{store: Store{DB: db}, admins: organizations.Admins{DB: db}} // no flag answer: off
	on := off.WithFlags(allOn)
	do := func(h System, route string, path map[string]string, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest("POST", "/", strings.NewReader(body)).WithContext(member)
		for k, v := range path {
			req.SetPathValue(k, v)
		}
		rec := httptest.NewRecorder()
		map[string]func(){"connection": func() { h.Connection(rec, req) }, "connect": func() { h.Connect(rec, req) }, "disconnect": func() { h.Disconnect(rec, req) },
			"connectURL": func() { h.ConnectURL(rec, req) }, "complete": func() { h.Complete(rec, req) }, "listRepos": func() { h.ListRepos(rec, req) },
			"projectRepos": func() { h.ProjectRepos(rec, req) }, "linkRepo": func() { h.LinkRepo(rec, req) }, "webhook": func() { h.BitbucketWebhook(rec, req) }}[route]()
		return rec
	}
	bb := map[string]string{"provider": "bitbucket"}
	for _, route := range []string{"connection", "connect", "disconnect", "connectURL", "complete", "listRepos"} {
		if rec := do(off, route, bb, `{}`); rec.Code != 404 {
			t.Errorf("%s with the flag off: %d %s", route, rec.Code, rec.Body)
		}
	}
	if rec := do(off, "webhook", map[string]string{"link": bbLink}, `{}`); rec.Code != 404 {
		t.Errorf("webhook with the flag off: %d", rec.Code)
	}
	if rec := do(off, "linkRepo", map[string]string{"id": pid}, `{"provider":"bitbucket","full_name":"acme/new"}`); rec.Code == 204 || rec.Code == 201 {
		t.Errorf("linked a Bitbucket repo with the flag off: %d", rec.Code)
	}
	repos := do(off, "projectRepos", map[string]string{"id": pid}, "").Body.String()
	if !strings.Contains(repos, "acme/web") || strings.Contains(repos, "acme/old") {
		t.Errorf("project repos with the flag off: %s", repos)
	}

	// GitHub isn't flagged; with the flag on, Bitbucket is back
	if rec := do(off, "connection", map[string]string{"provider": "github"}, ""); rec.Code != 200 {
		t.Errorf("github connection: %d", rec.Code)
	}
	if rec := do(on, "connection", bb, ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"connected":false`) {
		t.Errorf("bitbucket connection with the flag on: %d %s", rec.Code, rec.Body)
	}
	if repos := do(on, "projectRepos", map[string]string{"id": pid}, "").Body.String(); !strings.Contains(repos, "acme/old") {
		t.Errorf("project repos with the flag on: %s", repos)
	}
}
