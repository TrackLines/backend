package roadmaps

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/notifications"
	"github.com/tracklines/backend/internal/organizations"
)

// TL-413: only org admins and the project's team leaders change its roadmaps (agents count as their
// owner); updates say who posted them, by name, never by email.
func TestRoadmapWritesNeedAnAdminOrProjectLeader(t *testing.T) {
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

	const org = "rmm-org"
	for _, q := range []string{
		`DELETE FROM roadmaps WHERE owner_clerk_id IN ('rmm-org', 'rmm-other')`,
		`DELETE FROM boards WHERE owner_clerk_id = 'rmm-org'`,
		`DELETE FROM projects WHERE owner_clerk_id IN ('rmm-org', 'rmm-other')`,
		`DELETE FROM teams WHERE org_id = 'rmm-org'`,
		`DELETE FROM org_admins WHERE org_id IN ('rmm-org', 'rmm-other')`,
		`INSERT INTO org_admins (org_id, user_clerk_id, granted_by) VALUES ('rmm-org', 'user_admin', 'test'), ('rmm-other', 'user_x', 'test')`,
	} {
		if _, err := db.Exec(ctx, q); err != nil {
			t.Fatal(q, err)
		}
	}
	// project P: a board run by team T (leader user_lead, member user_mem); project Q: a board run by T2 (leader user_otherlead)
	var p, q, team, team2 string
	db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ($1, 'P') RETURNING id`, org).Scan(&p)
	db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ($1, 'Q') RETURNING id`, org).Scan(&q)
	db.QueryRow(ctx, `INSERT INTO teams (org_id, name) VALUES ($1, 'T') RETURNING id`, org).Scan(&team)
	db.QueryRow(ctx, `INSERT INTO teams (org_id, name) VALUES ($1, 'T2') RETURNING id`, org).Scan(&team2)
	for _, q := range []string{
		`INSERT INTO team_members (team_id, user_clerk_id, leader) VALUES ('` + team + `', 'user_lead', true), ('` + team + `', 'user_mem', false), ('` + team2 + `', 'user_otherlead', true)`,
		`INSERT INTO boards (project_id, owner_clerk_id, name, team_id) VALUES ('` + p + `', 'rmm-org', 'Backend', '` + team + `'), ('` + q + `', 'rmm-org', 'Other', '` + team2 + `')`,
	} {
		if _, err := db.Exec(ctx, q); err != nil {
			t.Fatal(q, err)
		}
	}
	rm, err := Store{DB: db}.Create(ctx, org, p, "Launch", "", Public)
	if err != nil {
		t.Fatal(err)
	}

	people := func(context.Context, string) ([]notifications.Person, error) {
		return []notifications.Person{
			{ID: "user_admin", Names: []string{"Ava Admin", "Ava"}},
			{ID: "user_lead", Names: []string{"lee@example.com", "lee"}}, // no name: shown without the domain
			{ID: "claude-1", Names: []string{"Claude"}},                  // an agent key, by its own name
		}, nil
	}
	h := NewSystem(db).WithManagers(organizations.Admins{DB: db}).WithPeople(people)
	as := func(user, inOrg string) context.Context { return auth.WithAPIKey(ctx, user, inOrg, user+"-key", "ai") }
	call := func(c context.Context, method, path, body string, handler http.HandlerFunc, ids ...string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(c)
		req.SetPathValue("id", ids[0])
		rec := httptest.NewRecorder()
		handler(rec, req)
		return rec
	}
	post := func(c context.Context) int {
		return call(c, http.MethodPost, "/", `{"health":"on_track","note":"Going well"}`, h.PublishUpdate, rm.ID).Code
	}
	items := func(c context.Context) int {
		return call(c, http.MethodPut, "/", `[{"title":"Beta"}]`, h.ReplaceItems, rm.ID).Code
	}
	create := func(c context.Context, project string) int {
		return call(c, http.MethodPost, "/", `{"title":"Next","visibility":"team"}`, h.Create, project).Code
	}

	for _, tc := range []struct {
		who, org     string
		update, item int
		create       int
	}{
		{"user_admin", org, http.StatusCreated, http.StatusNoContent, http.StatusCreated},
		{"user_lead", org, http.StatusCreated, http.StatusNoContent, http.StatusCreated},          // leads a team on this project
		{"user_mem", org, http.StatusForbidden, http.StatusForbidden, http.StatusForbidden},       // on the team, not its leader
		{"user_otherlead", org, http.StatusForbidden, http.StatusForbidden, http.StatusForbidden}, // leads another project's team
		{"user_x", "rmm-other", http.StatusNotFound, http.StatusNotFound, http.StatusNotFound},    // another org: not even found
	} {
		c := as(tc.who, tc.org)
		if got := post(c); got != tc.update {
			t.Errorf("%s posting an update: %d, want %d", tc.who, got, tc.update)
		}
		if got := items(c); got != tc.item {
			t.Errorf("%s changing items: %d, want %d", tc.who, got, tc.item)
		}
		if got := create(c, p); got != tc.create {
			t.Errorf("%s creating a roadmap: %d, want %d", tc.who, got, tc.create)
		}
	}
	// audit: a roadmap made without saying who sees it (API, MCP) is team only, never public by accident
	var made Roadmap
	json.Unmarshal(call(as("user_admin", org), http.MethodPost, "/", `{"title":"Unsaid"}`, h.Create, p).Body.Bytes(), &made)
	if made.Visibility != Team {
		t.Errorf("default visibility: %q", made.Visibility)
	}
	if code := call(as("user_mem", org), http.MethodDelete, "/", "", h.Delete, rm.ID).Code; code != http.StatusForbidden {
		t.Errorf("a member deleting: %d", code)
	}

	// a leader's agent can post too, and is credited as itself
	if code := call(auth.WithAPIKey(ctx, "user_lead", org, "claude-1", "ai"), http.MethodPost, "/", `{"health":"at_risk","note":"Slipping"}`, h.PublishUpdate, rm.ID).Code; code != http.StatusCreated {
		t.Fatalf("leader's agent posting: %d", code)
	}

	// who posted, by name; an email-only name loses its domain, even on a public roadmap
	rec := call(context.Background(), http.MethodGet, "/", "", h.ListUpdates, rm.ID)
	var got []Update
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got) != 3 {
		t.Fatalf("updates: %s %v", rec.Body, err)
	}
	if got[0].Author != "Ava Admin" || got[1].Author != "lee" || got[2].Author != "Claude" {
		t.Errorf("authors: %q, %q, %q", got[0].Author, got[1].Author, got[2].Author)
	}
	if strings.Contains(rec.Body.String(), "example.com") || strings.Contains(rec.Body.String(), "user_") {
		t.Errorf("public updates leaked an email or a user id: %s", rec.Body)
	}

	// can_manage tells the app whether to offer editing
	canManage := func(user string) bool {
		var r Roadmap
		json.Unmarshal(call(as(user, org), http.MethodGet, "/", "", h.Get, rm.ID).Body.Bytes(), &r)
		return r.CanManage
	}
	if !canManage("user_lead") || canManage("user_mem") {
		t.Errorf("can_manage: leader %v, member %v", canManage("user_lead"), canManage("user_mem"))
	}

	// without a managers model nobody can change roadmaps
	if code := call(as("user_admin", org), http.MethodPost, "/", `{"health":"on_track","note":"x"}`, NewSystem(db).PublishUpdate, rm.ID).Code; code != http.StatusForbidden {
		t.Errorf("no managers model: %d", code)
	}
}
