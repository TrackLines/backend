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

// fakeClerk is an org's members (oldest first), each with a Clerk role; Update changes a role and
// records it, and a non-member is Clerk's 404.
type fakeClerk struct {
	members *[][2]string // user id, role
	err     error
	updates *[]string // "user=role"
}

func newClerk(members ...[2]string) fakeClerk {
	return fakeClerk{members: &members, updates: &[]string{}}
}

func (f fakeClerk) List(_ context.Context, p *organizationmembership.ListParams) (*clerk.OrganizationMembershipList, error) {
	out := &clerk.OrganizationMembershipList{}
	for _, m := range *f.members {
		if (len(p.UserIDs) > 0 && !slices.Contains(p.UserIDs, m[0])) || (len(p.Roles) > 0 && !slices.Contains(p.Roles, m[1])) {
			continue
		}
		out.OrganizationMemberships = append(out.OrganizationMemberships, &clerk.OrganizationMembership{
			Role: m[1], PublicUserData: &clerk.OrganizationMembershipPublicUserData{UserID: m[0]},
		})
	}
	return out, f.err
}

func (f fakeClerk) Update(_ context.Context, p *organizationmembership.UpdateParams) (*clerk.OrganizationMembership, error) {
	if f.err != nil {
		return nil, f.err
	}
	for i, m := range *f.members {
		if m[0] == p.UserID {
			(*f.members)[i][1] = *p.Role
			*f.updates = append(*f.updates, p.UserID+"="+*p.Role)
			return &clerk.OrganizationMembership{Role: *p.Role}, nil
		}
	}
	return nil, &clerk.APIErrorResponse{HTTPStatusCode: 404}
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
	admins := func(f fakeClerk) Admins { return Admins{DB: db, Memberships: f, Roles: f} }
	ids := func(a Admins, org string) []string {
		t.Helper()
		list, err := a.List(ctx, org)
		if err != nil {
			t.Fatalf("%s: %v", org, err)
		}
		var out []string
		for _, ad := range list {
			out = append(out, ad.UserID+":"+ad.GrantedBy)
		}
		return out
	}

	// TL-690: an empty mirror is filled with Clerk's org:admin members (a new org's creator), nobody else
	if got := ids(admins(newClerk([2]string{"u0", "org:member"}, [2]string{"u1", "org:admin"}, [2]string{"u2", "org:admin"})), "org_creator"); !slices.Equal(got, []string{"u1:clerk:admin-role", "u2:clerk:admin-role"}) {
		t.Fatalf("seeded: %v", got)
	}
	// no Clerk admins: none, and no error
	if got := ids(admins(newClerk([2]string{"u4", "org:member"})), "org_none"); len(got) != 0 {
		t.Fatalf("no admins: %v", got)
	}
	// Clerk down: the error, nothing stored, so it retries
	down := errors.New("clerk down")
	if _, err := admins(fakeClerk{members: &[][2]string{}, err: down}).IsAdmin(ctx, "org_down", "u1"); !errors.Is(err, down) {
		t.Fatal("clerk error:", err)
	}
	if ok, err := admins(newClerk([2]string{"u9", "org:admin"})).IsAdmin(ctx, "org_down", "u9"); err != nil || !ok {
		t.Fatalf("retry after clerk error: %v %v", ok, err)
	}

	// grant and revoke change the Clerk role, then the mirror
	f := newClerk([2]string{"u1", "org:admin"}, [2]string{"u2", "org:admin"}, [2]string{"u6", "org:member"})
	a := admins(f)
	if _, err := db.Exec(ctx, `DELETE FROM org_admins WHERE org_id = 'org_creator' AND user_clerk_id = 'u2'`); err != nil {
		t.Fatal(err)
	}
	if err := a.Grant(ctx, "org_creator", "outsider", "u1"); !errors.Is(err, ErrNotMember) {
		t.Fatal("grant non-member:", err)
	}
	if err := a.Grant(ctx, "org_creator", "u6", "u1"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := a.IsAdmin(ctx, "org_creator", "u6"); !ok || !slices.Contains(*f.updates, "u6=org:admin") {
		t.Fatalf("u6 should be admin, in Clerk too: %v", *f.updates)
	}
	if err := a.Revoke(ctx, "org_creator", "u1"); err != nil || !slices.Contains(*f.updates, "u1=org:member") {
		t.Fatalf("revoke: %v %v", err, *f.updates)
	}
	if err := a.Revoke(ctx, "org_creator", "u6"); !errors.Is(err, ErrLastAdmin) {
		t.Fatal("last admin:", err)
	}
	if err := a.Revoke(ctx, "org_creator", "u0"); err != nil { // not an admin: nothing to do
		t.Fatal("revoke non-admin:", err)
	}
	*f.updates = nil
	if err := admins(fakeClerk{members: f.members, updates: f.updates, err: down}).Grant(ctx, "org_creator", "u2", "u6"); !errors.Is(err, down) {
		t.Fatal("grant with clerk down:", err)
	}
	if ok, _ := a.IsAdmin(ctx, "org_creator", "u2"); ok {
		t.Fatal("a failed Clerk change mustn't make an admin")
	}
	// the one-off sync: admins recorded before TL-690 get the Clerk role once; a leaver loses the row;
	// the org's other Clerk admins join the mirror
	sync := newClerk([2]string{"s1", "org:member"}, [2]string{"s3", "org:admin"})
	if _, err := db.Exec(ctx, `INSERT INTO org_admins (org_id, user_clerk_id, granted_by, in_clerk) VALUES
		('org_sync', 's1', 'clerk:creator', false), ('org_sync', 's2', 's1', false)`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := admins(sync).SyncToClerk(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := ids(admins(sync), "org_sync"); !slices.Equal(got, []string{"s1:clerk:creator", "s3:clerk:admin-role"}) || !slices.Equal(*sync.updates, []string{"s1=org:admin"}) {
		t.Fatalf("synced: %v, clerk updates %v", got, *sync.updates)
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
	// the yes/no form (assign on move, TL-266): the leader of this board's team, not members, not elsewhere
	for _, c := range []struct {
		org, user, board string
		want             bool
	}{{"org_creator", "leader", boardID, true}, {"org_creator", "member", boardID, false}, {"org_other", "leader", boardID, false}} {
		if got, err := a.IsBoardManager(ctx, c.org, c.user, c.board); err != nil || got != c.want {
			t.Fatalf("IsBoardManager(%s, %s) = %v, %v; want %v", c.org, c.user, got, err, c.want)
		}
	}
	// refinement: approving a planned sprint follows the same rule
	var plannedID string
	if err := db.QueryRow(ctx, `INSERT INTO planned_sprints(board_id,position,length_days) VALUES ($1,1,14) RETURNING id`, boardID).Scan(&plannedID); err != nil {
		t.Fatal(err)
	}
	if code := manager("leader", false, "planned_sprint", plannedID); code != 200 {
		t.Fatalf("team leader planned sprint access: %d", code)
	}
	if code := manager("member", false, "planned_sprint", plannedID); code != 403 {
		t.Fatalf("ordinary member planned sprint access: %d", code)
	}
}
