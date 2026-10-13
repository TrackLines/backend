package teams

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/organizations"
)

// Team handlers: create / rename / delete (admins), members and removal, linking teams to
// projects; another org never sees or changes them.
func TestTeamHandlers(t *testing.T) {
	ctx := context.Background()
	url := os.Getenv("TEST_DATABASE_URL")
	m, err := migrate.New("file://../migrations", "pgx5"+url[len("postgres"):])
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatal(err)
	}
	db, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const org, other = "org_th1", "org_th2"
	for _, q := range []string{
		`DELETE FROM teams WHERE org_id IN ($1, $2)`, `DELETE FROM projects WHERE owner_clerk_id IN ($1, $2)`,
		`DELETE FROM org_admins WHERE org_id IN ($1, $2)`,
		`INSERT INTO org_admins (org_id, user_clerk_id, granted_by) VALUES ($1, 'admin-u', 'test'), ($2, 'admin-x', 'test')`,
	} {
		if _, err := db.Exec(ctx, q, org, other); err != nil {
			t.Fatal(err)
		}
	}
	var pid, otherPid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ($1, 'Mine') RETURNING id`, org).Scan(&pid)
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ($1, 'Theirs') RETURNING id`, other).Scan(&otherPid)

	h := NewSystem(db, organizations.Admins{DB: db})
	run := func(action, o, user string, path map[string]string, body string) *httptest.ResponseRecorder {
		t.Helper()
		c := &clerk.SessionClaims{RegisteredClaims: clerk.RegisteredClaims{Subject: user}}
		c.ActiveOrganizationID = o
		req := httptest.NewRequest("POST", "/", strings.NewReader(body)).WithContext(clerk.ContextWithSessionClaims(ctx, c))
		for k, v := range path {
			req.SetPathValue(k, v)
		}
		rec := httptest.NewRecorder()
		map[string]func(){"create": func() { h.Create(rec, req) }, "update": func() { h.Update(rec, req) }, "delete": func() { h.Delete(rec, req) },
			"members": func() { h.Members(rec, req) }, "remove": func() { h.RemoveMember(rec, req) },
			"projectTeams": func() { h.ProjectTeams(rec, req) }, "link": func() { h.AddProjectTeam(rec, req) }}[action]()
		return rec
	}
	expect := func(want int, action, o, user string, path map[string]string, body string) *httptest.ResponseRecorder {
		t.Helper()
		rec := run(action, o, user, path, body)
		if rec.Code != want {
			t.Fatalf("%s as %s/%s %v %s: %d %s, want %d", action, o, user, path, body, rec.Code, rec.Body, want)
		}
		return rec
	}
	id := func(team string) map[string]string { return map[string]string{"id": team} }
	const missing = "00000000-0000-0000-0000-000000000000"

	// create: admins only, a name is required
	expect(403, "create", org, "member-u", nil, `{"name":"Backend"}`)
	expect(400, "create", org, "admin-u", nil, `{"name":"  "}`)
	var team Team
	_ = json.Unmarshal(expect(201, "create", org, "admin-u", nil, `{"name":" Backend ","description":"APIs"}`).Body.Bytes(), &team)
	if team.Name != "Backend" || team.OrgID != org || team.Description != "APIs" {
		t.Fatalf("created: %+v", team)
	}

	// rename: admins only, in their own org
	expect(204, "update", org, "admin-u", id(team.ID), `{"name":"Platform","description":"APIs and infra"}`)
	var name string
	_ = db.QueryRow(ctx, `SELECT name FROM teams WHERE id = $1`, team.ID).Scan(&name)
	if name != "Platform" {
		t.Fatalf("renamed: %s", name)
	}
	expect(400, "update", org, "admin-u", id(team.ID), `{"name":""}`)
	expect(403, "update", org, "member-u", id(team.ID), `{"name":"x"}`)
	expect(404, "update", other, "admin-x", id(team.ID), `{"name":"hijack"}`)
	expect(404, "update", org, "admin-u", id(missing), `{"name":"x"}`)

	// members: any member of the org can read them; removal is admin-only
	if _, err := db.Exec(ctx, `INSERT INTO team_members (team_id, user_clerk_id, leader) VALUES ($1, 'ava', true), ($1, 'bo', false)`, team.ID); err != nil {
		t.Fatal(err)
	}
	var members []Member
	_ = json.Unmarshal(expect(200, "members", org, "member-u", id(team.ID), "").Body.Bytes(), &members)
	if len(members) != 2 || members[0].UserID != "ava" || !members[0].Leader || members[1].Leader {
		t.Fatalf("members: %+v", members)
	}
	expect(404, "members", other, "admin-x", id(team.ID), "")
	bo := map[string]string{"id": team.ID, "userID": "bo"}
	expect(403, "remove", org, "member-u", bo, "")
	expect(404, "remove", other, "admin-x", bo, "")
	expect(204, "remove", org, "admin-u", bo, "")
	_ = json.Unmarshal(expect(200, "members", org, "member-u", id(team.ID), "").Body.Bytes(), &members)
	if len(members) != 1 || members[0].UserID != "ava" {
		t.Fatalf("after removal: %+v", members)
	}

	// link to a project: admins only, only within the org; listed on the project
	link := map[string]string{"projectID": pid, "teamID": team.ID}
	expect(403, "link", org, "member-u", link, "")
	expect(404, "link", org, "admin-u", map[string]string{"projectID": otherPid, "teamID": team.ID}, "")
	expect(404, "link", other, "admin-x", link, "")
	expect(204, "link", org, "admin-u", link, "")
	expect(204, "link", org, "admin-u", link, "") // again: already linked
	var linked []Team
	_ = json.Unmarshal(expect(200, "projectTeams", org, "member-u", map[string]string{"projectID": pid}, "").Body.Bytes(), &linked)
	if len(linked) != 1 || linked[0].ID != team.ID {
		t.Fatalf("project teams: %+v", linked)
	}
	if rec := expect(200, "projectTeams", other, "admin-x", map[string]string{"projectID": pid}, ""); strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("another org's view of the project's teams: %s", rec.Body)
	}

	// delete: admins only, in their own org; takes the members and project links with it
	expect(403, "delete", org, "member-u", id(team.ID), "")
	expect(404, "delete", other, "admin-x", id(team.ID), "")
	expect(204, "delete", org, "admin-u", id(team.ID), "")
	expect(404, "delete", org, "admin-u", id(team.ID), "")
	var left int
	_ = db.QueryRow(ctx, `SELECT (SELECT count(*) FROM team_members WHERE team_id = $1) + (SELECT count(*) FROM project_teams WHERE team_id = $1)`, team.ID).Scan(&left)
	if left != 0 {
		t.Fatalf("members and links left behind: %d", left)
	}
}
