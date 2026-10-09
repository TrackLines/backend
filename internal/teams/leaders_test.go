package teams

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/organizations"
)

func TestLeadersAndBoardTeams(t *testing.T) {
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
	const org = "lead-org"
	var project, board, otherBoard, team string
	if _, err := db.Exec(ctx, `DELETE FROM teams WHERE org_id IN ('lead-org', 'lead-other'); DELETE FROM boards WHERE owner_clerk_id IN ('lead-org', 'lead-other');
		DELETE FROM projects WHERE owner_clerk_id IN ('lead-org', 'lead-other'); DELETE FROM org_admins WHERE org_id = 'lead-org';
		INSERT INTO org_admins (org_id, user_clerk_id, granted_by) VALUES ('lead-org', 'admin-u', 'test')`); err != nil {
		t.Fatal(err)
	}
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ($1, 'p') RETURNING id`, org).Scan(&project)
	_ = db.QueryRow(ctx, `INSERT INTO boards (project_id, owner_clerk_id, name) VALUES ($1, $2, 'b') RETURNING id`, project, org).Scan(&board)
	var otherProject string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('lead-other', 'x') RETURNING id`).Scan(&otherProject)
	_ = db.QueryRow(ctx, `INSERT INTO boards (project_id, owner_clerk_id, name) VALUES ($1, 'lead-other', 'x') RETURNING id`, otherProject).Scan(&otherBoard)
	_ = db.QueryRow(ctx, `INSERT INTO teams (org_id, name) VALUES ($1, 'Backend') RETURNING id`, org).Scan(&team)
	_, _ = db.Exec(ctx, `INSERT INTO team_members (team_id, user_clerk_id) VALUES ($1, 'member-u'), ($1, 'admin-u')`, team)

	h := NewSystem(db, organizations.Admins{DB: db}) // admins are pre-seeded, so Clerk isn't called
	as := func(user string) context.Context {
		c := &clerk.SessionClaims{RegisteredClaims: clerk.RegisteredClaims{Subject: user}}
		c.ActiveOrganizationID = org
		return clerk.ContextWithSessionClaims(ctx, c)
	}
	call := func(f func(http.ResponseWriter, *http.Request), user, body string, path map[string]string) int {
		req := httptest.NewRequest("PUT", "/", strings.NewReader(body)).WithContext(as(user))
		for k, v := range path {
			req.SetPathValue(k, v)
		}
		rec := httptest.NewRecorder()
		f(rec, req)
		return rec.Code
	}
	leader := map[string]string{"id": team, "userID": "member-u"}

	if code := call(h.AddLeader, "member-u", "", leader); code != 403 {
		t.Fatalf("non-admin names a leader: %d", code)
	}
	if code := call(h.AddLeader, "admin-u", "", map[string]string{"id": team, "userID": "not-on-team"}); code != 422 {
		t.Fatalf("leader must be on the team: %d", code)
	}
	if code := call(h.AddLeader, "admin-u", "", leader); code != 204 {
		t.Fatalf("add leader: %d", code)
	}
	var lead bool
	_ = db.QueryRow(ctx, `SELECT leader FROM team_members WHERE team_id = $1 AND user_clerk_id = 'member-u'`, team).Scan(&lead)
	if !lead {
		t.Fatal("member-u should lead")
	}

	body := `{"team_id":"` + team + `"}`
	if code := call(h.SetBoardTeam, "admin-u", body, map[string]string{"id": board}); code != 422 {
		t.Fatalf("team not on the project yet: %d", code)
	}
	_, _ = db.Exec(ctx, `INSERT INTO project_teams (project_id, team_id, org_id) VALUES ($1, $2, $3)`, project, team, org)
	if code := call(h.SetBoardTeam, "member-u", body, map[string]string{"id": board}); code != 403 {
		t.Fatalf("non-admin links a board: %d", code)
	}
	if code := call(h.SetBoardTeam, "admin-u", body, map[string]string{"id": otherBoard}); code != 404 {
		t.Fatalf("another org's board: %d", code)
	}
	if code := call(h.SetBoardTeam, "admin-u", body, map[string]string{"id": board}); code != 204 {
		t.Fatalf("link board: %d", code)
	}
	boardTeam := func() *string {
		var id *string
		_ = db.QueryRow(ctx, `SELECT team_id::text FROM boards WHERE id = $1`, board).Scan(&id)
		return id
	}
	if id := boardTeam(); id == nil || *id != team {
		t.Fatalf("board team: %v", id)
	}
	// taking the team off the project unlinks its boards
	req := httptest.NewRequest("DELETE", "/", nil).WithContext(as("admin-u"))
	req.SetPathValue("projectID", project)
	req.SetPathValue("teamID", team)
	h.RemoveProjectTeam(httptest.NewRecorder(), req)
	if id := boardTeam(); id != nil {
		t.Fatalf("board should be unlinked, got %v", *id)
	}
}
