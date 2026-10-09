package teams

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/clerk/clerk-sdk-go/v2/organizationmembership"
	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
)

func TestTeamAndProjectMembershipsStayWithinOrganization(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Fatal("TEST_DATABASE_URL not set")
	}
	m, err := migrate.New("file://../migrations", "pgx5"+url[len("postgres"):])
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatal(err)
	}
	db, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err = db.Exec(ctx, `DELETE FROM teams WHERE org_id IN ('team-org-a','team-org-b'); DELETE FROM projects WHERE owner_clerk_id IN ('team-org-a','team-org-b'); INSERT INTO projects(owner_clerk_id,name) VALUES ('team-org-a','A'),('team-org-b','B')`); err != nil {
		t.Fatal(err)
	}
	var projectA, projectB, teamA string
	if err = db.QueryRow(ctx, `SELECT id FROM projects WHERE owner_clerk_id='team-org-a'`).Scan(&projectA); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(ctx, `SELECT id FROM projects WHERE owner_clerk_id='team-org-b'`).Scan(&projectB); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(ctx, `INSERT INTO teams(org_id,name) VALUES ('team-org-a','Backend') RETURNING id`).Scan(&teamA); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(ctx, `INSERT INTO team_members(team_id,user_clerk_id) VALUES ($1,'user-a'),($1,'user-b')`, teamA); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(ctx, `INSERT INTO project_teams(project_id,team_id,org_id) VALUES ($1,$2,'team-org-a')`, projectA, teamA); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(ctx, `INSERT INTO project_teams(project_id,team_id,org_id) VALUES ($1,$2,'team-org-b')`, projectB, teamA); err == nil {
		t.Fatal("cross-organization project/team link was accepted")
	}

	h := NewSystem(db)
	r := httptest.NewRequest(http.MethodGet, "/api/teams", nil)
	r = r.WithContext(auth.WithAPIKey(r.Context(), "user-a", "team-org-b", "test", "ai"))
	w := httptest.NewRecorder()
	h.List(w, r)
	if w.Code != http.StatusOK || w.Body.String() != "[]\n" {
		t.Fatalf("cross-org list: %d %s", w.Code, w.Body.String())
	}

	h.memberships = fakeMemberships{result: &clerk.OrganizationMembershipList{OrganizationMemberships: []*clerk.OrganizationMembership{{PublicUserData: &clerk.OrganizationMembershipPublicUserData{UserID: "user-c"}}}}}
	add := httptest.NewRequest(http.MethodPut, "/api/teams/"+teamA+"/members/user-c", nil)
	add.SetPathValue("id", teamA)
	add.SetPathValue("userID", "user-c")
	add = add.WithContext(auth.WithAPIKey(add.Context(), "user-a", "team-org-a", "test", "ai"))
	w = httptest.NewRecorder()
	h.AddMember(w, add)
	if w.Code != http.StatusNoContent {
		t.Fatalf("add org member: %d %s", w.Code, w.Body.String())
	}

	h.memberships = fakeMemberships{result: &clerk.OrganizationMembershipList{}}
	add = httptest.NewRequest(http.MethodPut, "/api/teams/"+teamA+"/members/outsider", nil)
	add.SetPathValue("id", teamA)
	add.SetPathValue("userID", "outsider")
	add = add.WithContext(auth.WithAPIKey(add.Context(), "user-a", "team-org-a", "test", "ai"))
	w = httptest.NewRecorder()
	h.AddMember(w, add)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("add non-member: %d %s", w.Code, w.Body.String())
	}
}

type fakeMemberships struct {
	result *clerk.OrganizationMembershipList
	err    error
}

func (f fakeMemberships) List(context.Context, *organizationmembership.ListParams) (*clerk.OrganizationMembershipList, error) {
	return f.result, f.err
}
