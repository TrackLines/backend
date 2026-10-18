package roadmaps

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
)

// GET /api/roadmaps lists only the active org's roadmaps; another org, or no org, sees none of them.
func TestListIsOrgScoped(t *testing.T) {
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
	if _, err := db.Exec(ctx, `DELETE FROM roadmaps WHERE owner_clerk_id IN ('org_rl1', 'org_rl2');
		DELETE FROM projects WHERE owner_clerk_id IN ('org_rl1', 'org_rl2')`); err != nil {
		t.Fatal(err)
	}
	s := Store{DB: db}
	roadmap := func(org, title string) {
		var pid string
		if err := db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ($1, $2) RETURNING id`, org, title+" project").Scan(&pid); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Create(ctx, org, pid, title, "", Public); err != nil {
			t.Fatal(err)
		}
	}
	roadmap("org_rl1", "Ours A")
	roadmap("org_rl1", "Ours B")
	roadmap("org_rl2", "Theirs")

	h := NewSystem(db).WithManagers(everyone{})
	list := func(org string) []string {
		t.Helper()
		req := httptest.NewRequest("GET", "/api/roadmaps", nil).WithContext(auth.WithAPIKey(ctx, "user_rl", org, "ui", ""))
		rec := httptest.NewRecorder()
		h.List(rec, req)
		var out []Roadmap
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
			t.Fatalf("list %q: %d %s", org, rec.Code, rec.Body)
		}
		var titles []string
		for _, r := range out {
			titles = append(titles, r.Title)
		}
		return titles
	}
	if got := list("org_rl1"); len(got) != 2 {
		t.Fatalf("our org: %v", got)
	}
	if got := list("org_rl2"); len(got) != 1 || got[0] != "Theirs" {
		t.Fatalf("their org: %v", got)
	}
	if got := list("org_rl3"); len(got) != 0 {
		t.Fatalf("an org with none: %v", got)
	}
	if got := list(""); len(got) != 0 {
		t.Fatalf("no active org: %v", got)
	}
}

// PUT and DELETE /api/roadmaps/{id}: the owning org can change and delete a roadmap; another org can't.
func TestUpdateDeleteAreOrgScoped(t *testing.T) {
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
	if _, err := db.Exec(ctx, `DELETE FROM roadmaps WHERE owner_clerk_id IN ('org_ru1', 'org_ru2'); DELETE FROM projects WHERE owner_clerk_id IN ('org_ru1', 'org_ru2')`); err != nil {
		t.Fatal(err)
	}
	var pid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('org_ru1', 'Roadmaps') RETURNING id`).Scan(&pid)
	s := Store{DB: db}
	rm, err := s.Create(ctx, "org_ru1", pid, "Q4", "", Public)
	if err != nil {
		t.Fatal(err)
	}
	h := NewSystem(db).WithManagers(everyone{})
	run := func(action, org, body string) int {
		c := &clerk.SessionClaims{RegisteredClaims: clerk.RegisteredClaims{Subject: "user_ru"}}
		c.ActiveOrganizationID = org // signed in: unpublishing and deleting aren't for API keys (TL-432)
		req := httptest.NewRequest("PUT", "/", strings.NewReader(body)).WithContext(clerk.ContextWithSessionClaims(ctx, c))
		req.SetPathValue("id", rm.ID)
		rec := httptest.NewRecorder()
		if action == "update" {
			h.Update(rec, req)
		} else {
			h.Delete(rec, req)
		}
		return rec.Code
	}
	for _, c := range []struct {
		action, org, body string
		want              int
	}{
		{"update", "org_ru2", `{"title":"hijacked"}`, 404},
		{"update", "org_ru1", `{"title":"  "}`, 400},
		{"update", "org_ru1", `{"title":"Q4","visibility":"everyone"}`, 400},
		{"update", "org_ru1", `{"title":"Q4 plan","description":"what ships","visibility":"login_only"}`, 204},
		{"delete", "org_ru2", ``, 404},
	} {
		if got := run(c.action, c.org, c.body); got != c.want {
			t.Fatalf("%s as %s %s: %d, want %d", c.action, c.org, c.body, got, c.want)
		}
	}
	got, err := s.Get(ctx, rm.ID)
	if err != nil || got.Title != "Q4 plan" || got.Description != "what ships" || got.Visibility != LoginOnly {
		t.Fatalf("updated: %+v %v", got, err)
	}
	if code := run("delete", "org_ru1", ""); code != 204 {
		t.Fatalf("delete: %d", code)
	}
	if _, err := s.Get(ctx, rm.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted roadmap still there: %v", err)
	}
	if code := run("delete", "org_ru1", ""); code != 404 {
		t.Fatalf("delete again: %d", code)
	}
}
