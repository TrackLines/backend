package organizations

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"slices"
	"testing"

	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/clerk/clerk-sdk-go/v2/organizationmembership"
	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
)

// fakeClerk is an org with a creator and members (oldest first), each with a Clerk role.
type fakeClerk struct {
	creator string
	members [][2]string // user id, role
	err     error
}

func (f fakeClerk) Get(context.Context, string) (*clerk.Organization, error) {
	return &clerk.Organization{CreatedBy: f.creator}, f.err
}

func (f fakeClerk) List(_ context.Context, p *organizationmembership.ListParams) (*clerk.OrganizationMembershipList, error) {
	out := &clerk.OrganizationMembershipList{}
	for _, m := range f.members {
		if (len(p.UserIDs) > 0 && !slices.Contains(p.UserIDs, m[0])) || (len(p.Roles) > 0 && !slices.Contains(p.Roles, m[1])) {
			continue
		}
		out.OrganizationMemberships = append(out.OrganizationMemberships, &clerk.OrganizationMembership{
			Role: m[1], PublicUserData: &clerk.OrganizationMembershipPublicUserData{UserID: m[0]},
		})
	}
	return out, f.err
}

func TestAdmins(t *testing.T) {
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
	admins := func(f fakeClerk) Admins { return Admins{DB: db, Orgs: f, Memberships: f} }
	first := func(a Admins, org string) Admin {
		t.Helper()
		list, err := a.List(ctx, org)
		if err != nil || len(list) != 1 {
			t.Fatalf("%s: %+v %v", org, list, err)
		}
		return list[0]
	}

	// 1. the creator, while still a member
	if ad := first(admins(fakeClerk{creator: "u1", members: [][2]string{{"u0", "org:admin"}, {"u1", "org:member"}}}), "org_creator"); ad.UserID != "u1" || ad.GrantedBy != "clerk:creator" {
		t.Fatalf("creator: %+v", ad)
	}
	// 2. creator gone: the longest-standing Clerk admin
	if ad := first(admins(fakeClerk{creator: "gone", members: [][2]string{{"u0", "org:member"}, {"u2", "org:admin"}, {"u3", "org:admin"}}}), "org_role"); ad.UserID != "u2" || ad.GrantedBy != "clerk:admin-role" {
		t.Fatalf("admin role: %+v", ad)
	}
	// 3. no Clerk admins either: the longest-standing member
	if ad := first(admins(fakeClerk{members: [][2]string{{"u4", "org:member"}, {"u5", "org:member"}}}), "org_earliest"); ad.UserID != "u4" || ad.GrantedBy != "clerk:earliest-member" {
		t.Fatalf("earliest: %+v", ad)
	}
	// once resolved it sticks, even if Clerk changes
	if ad := first(admins(fakeClerk{creator: "someone-else", members: [][2]string{{"someone-else", "org:admin"}}}), "org_creator"); ad.UserID != "u1" {
		t.Fatalf("seed should be stored: %+v", ad)
	}
	// 4. nobody to pick, or Clerk down: an error and nothing stored, so it retries
	if _, err := admins(fakeClerk{}).List(ctx, "org_empty"); !errors.Is(err, ErrNoMembers) {
		t.Fatal("empty org:", err)
	}
	down := errors.New("clerk down")
	if _, err := admins(fakeClerk{err: down}).IsAdmin(ctx, "org_down", "u1"); !errors.Is(err, down) {
		t.Fatal("clerk error:", err)
	}
	if ad := first(admins(fakeClerk{creator: "u9", members: [][2]string{{"u9", "org:admin"}}}), "org_down"); ad.UserID != "u9" {
		t.Fatalf("retry after clerk error: %+v", ad)
	}

	// grant and revoke
	a := admins(fakeClerk{creator: "u1", members: [][2]string{{"u1", "org:admin"}, {"u6", "org:member"}}})
	if err := a.Grant(ctx, "org_creator", "outsider", "u1"); !errors.Is(err, ErrNotMember) {
		t.Fatal("grant non-member:", err)
	}
	if err := a.Grant(ctx, "org_creator", "u6", "u1"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := a.IsAdmin(ctx, "org_creator", "u6"); !ok {
		t.Fatal("u6 should be admin")
	}
	if err := a.Revoke(ctx, "org_creator", "u1"); err != nil {
		t.Fatal(err)
	}
	if err := a.Revoke(ctx, "org_creator", "u6"); !errors.Is(err, ErrLastAdmin) {
		t.Fatal("last admin:", err)
	}

	// handlers: API keys and non-admins can't grant; the admin can
	h := System{admins: a}
	grant := func(ctx context.Context) int {
		req := httptest.NewRequest("PUT", "/", nil).WithContext(ctx)
		req.SetPathValue("userID", "u1")
		rec := httptest.NewRecorder()
		h.GrantAdmin(rec, req)
		return rec.Code
	}
	session := func(user string) context.Context {
		return clerk.ContextWithSessionClaims(ctx, func() *clerk.SessionClaims {
			c := &clerk.SessionClaims{RegisteredClaims: clerk.RegisteredClaims{Subject: user}}
			c.ActiveOrganizationID = "org_creator"
			return c
		}())
	}
	if code := grant(auth.WithAPIKey(ctx, "u6", "org_creator", "agent", "ai")); code != 403 {
		t.Fatalf("api key grant: %d", code)
	}
	if code := grant(session("u1")); code != 403 { // u1 was revoked above
		t.Fatalf("non-admin grant: %d", code)
	}
	if code := grant(session("u6")); code != 204 {
		t.Fatalf("admin grant: %d", code)
	}

	// A team leader can manage only the board assigned to their team; ordinary members and
	// API keys cannot manage the board workflow.
	if _, err := db.Exec(ctx, `INSERT INTO users(clerk_id,email) VALUES ('u6','u6@example.test'),('leader','leader@example.test'),('member','member@example.test') ON CONFLICT (clerk_id) DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	var projectID, boardID, teamID, columnID string
	if err := db.QueryRow(ctx, `INSERT INTO projects(owner_clerk_id,name) VALUES ('org_creator','RBAC test') RETURNING id`).Scan(&projectID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `INSERT INTO boards(owner_clerk_id,name,project_id) VALUES ('org_creator','RBAC test', $1) RETURNING id`, projectID).Scan(&boardID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `INSERT INTO teams(org_id,name) VALUES ('org_creator','RBAC test') RETURNING id`).Scan(&teamID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO team_members(team_id,user_clerk_id,leader) VALUES ($1,'leader',true)`, teamID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `UPDATE boards SET team_id=$1 WHERE id=$2`, teamID, boardID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `INSERT INTO columns(board_id,name,position) VALUES ($1,'Todo',0) RETURNING id`, boardID).Scan(&columnID); err != nil {
		t.Fatal(err)
	}
	manager := func(user string, apiKey bool, resource string, id string) int {
		r := httptest.NewRequest("PATCH", "/", nil)
		if apiKey {
			r = r.WithContext(auth.WithAPIKey(r.Context(), user, "org_creator", "test", "ai"))
		} else {
			r = r.WithContext(session(user))
		}
		w := httptest.NewRecorder()
		if resource == "board" {
			a.RequireResourceBoardManager(w, r, "board", id)
		} else {
			a.RequireResourceBoardManager(w, r, resource, id)
		}
		return w.Code
	}
	if code := manager("leader", false, "board", boardID); code != 200 {
		t.Fatalf("team leader board access: %d", code)
	}
	if code := manager("leader", false, "column", columnID); code != 200 {
		t.Fatalf("team leader column access: %d", code)
	}
	if code := manager("member", false, "board", boardID); code != 403 {
		t.Fatalf("ordinary member board access: %d", code)
	}
	if code := manager("leader", true, "board", boardID); code != 403 {
		t.Fatalf("API key board access: %d", code)
	}
}
