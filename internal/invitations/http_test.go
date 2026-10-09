package invitations

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/clerk/clerk-sdk-go/v2/organizationinvitation"
	"github.com/clerk/clerk-sdk-go/v2/organizationmembership"
	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
	"github.com/tracklines/backend/internal/organizations"
)

type fakeInvites struct {
	invitation *clerk.OrganizationInvitation
	next       int
}

func (f *fakeInvites) Create(_ context.Context, p *organizationinvitation.CreateParams) (*clerk.OrganizationInvitation, error) {
	f.next++
	copy := *f.invitation
	copy.ID = fmt.Sprintf("orginv_test_%d", f.next)
	copy.EmailAddress = *p.EmailAddress
	copy.OrganizationID = p.OrganizationID
	copy.Status = "pending"
	f.invitation = &copy
	return f.invitation, nil
}
func (f *fakeInvites) Get(context.Context, *organizationinvitation.GetParams) (*clerk.OrganizationInvitation, error) {
	return f.invitation, nil
}
func (f *fakeInvites) Revoke(context.Context, *organizationinvitation.RevokeParams) (*clerk.OrganizationInvitation, error) {
	f.invitation.Status = "revoked"
	return f.invitation, nil
}

type fakeMemberships struct{}

func (fakeMemberships) List(_ context.Context, p *organizationmembership.ListParams) (*clerk.OrganizationMembershipList, error) {
	if len(p.UserIDs) > 0 {
		return &clerk.OrganizationMembershipList{OrganizationMemberships: []*clerk.OrganizationMembership{{PublicUserData: &clerk.OrganizationMembershipPublicUserData{UserID: p.UserIDs[0]}}}}, nil
	}
	return &clerk.OrganizationMembershipList{}, nil
}

type fakeUsers struct{}

func (fakeUsers) Get(context.Context, string) (*clerk.User, error) {
	return &clerk.User{EmailAddresses: []*clerk.EmailAddress{{EmailAddress: "invitee@example.test", Verification: &clerk.Verification{Status: "verified"}}}}, nil
}

func TestInvitationAcceptanceGrantsScopedMembership(t *testing.T) {
	ctx := context.Background()
	url := os.Getenv("TEST_DATABASE_URL")
	m, err := migrate.New("file://../migrations", "pgx5"+url[len("postgres"):])
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatal(err)
	}
	db, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(ctx, `DELETE FROM projects WHERE owner_clerk_id='invite-org'; DELETE FROM teams WHERE org_id='invite-org'; DELETE FROM invitations WHERE org_id='invite-org'; DELETE FROM org_admins WHERE org_id='invite-org';
		INSERT INTO org_admins(org_id,user_clerk_id,granted_by) VALUES('invite-org','admin-user','test');
		INSERT INTO users(clerk_id,email) VALUES('admin-user','admin@example.test') ON CONFLICT(clerk_id) DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	var teamID, projectID string
	if err := db.QueryRow(ctx, `INSERT INTO teams(org_id,name) VALUES('invite-org','Engineering') RETURNING id`).Scan(&teamID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `INSERT INTO projects(owner_clerk_id,name) VALUES('invite-org','Client') RETURNING id`).Scan(&projectID); err != nil {
		t.Fatal(err)
	}
	clerkInvite := &clerk.OrganizationInvitation{
		ID: "orginv_test", OrganizationID: "invite-org", EmailAddress: "invitee@example.test",
		Role: "org:member", URL: "https://clerk.example/invite", Status: "pending",
		ExpiresAt: clerk.Int64(time.Now().Add(7 * 24 * time.Hour).UnixMilli()),
	}
	h := NewSystem(db, organizations.Admins{DB: db})
	h.invitations = &fakeInvites{invitation: clerkInvite}
	h.memberships = fakeMemberships{}
	h.users = fakeUsers{}

	admin := clerk.ContextWithSessionClaims(ctx, func() *clerk.SessionClaims {
		c := &clerk.SessionClaims{RegisteredClaims: clerk.RegisteredClaims{Subject: "admin-user"}}
		c.ActiveOrganizationID = "invite-org"
		return c
	}())
	create := func(scope string, target *string) Invitation {
		t.Helper()
		payload, _ := json.Marshal(inviteInput{EmailAddress: "invitee@example.test", Scope: scope, TargetID: target})
		req := httptest.NewRequest(http.MethodPost, "/api/organizations/invitations", strings.NewReader(string(payload))).WithContext(admin)
		w := httptest.NewRecorder()
		h.Create(w, req)
		if w.Code != http.StatusCreated {
			t.Fatalf("create %s invite: %d %s", scope, w.Code, w.Body.String())
		}
		var out Invitation
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	accept := func(item Invitation) {
		t.Helper()
		invitee := clerk.ContextWithSessionClaims(ctx, &clerk.SessionClaims{RegisteredClaims: clerk.RegisteredClaims{Subject: "invitee-user"}})
		mineReq := httptest.NewRequest(http.MethodGet, "/api/invitations", nil).WithContext(invitee)
		mineRec := httptest.NewRecorder()
		h.Mine(mineRec, mineReq)
		if mineRec.Code != http.StatusOK {
			t.Fatalf("list recipient invitations: %d %s", mineRec.Code, mineRec.Body.String())
		}
		var mine []Invitation
		if err := json.Unmarshal(mineRec.Body.Bytes(), &mine); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, foundInvite := range mine {
			if foundInvite.ID == item.ID {
				found = true
			}
		}
		if !found {
			t.Fatalf("recipient invitation list omitted %s invite", item.Scope)
		}
		h.invitations.(*fakeInvites).invitation.Status = "accepted"
		req := httptest.NewRequest(http.MethodPost, "/api/organizations/invitations/"+item.ID+"/accept", nil).WithContext(invitee)
		req.SetPathValue("id", item.ID)
		w := httptest.NewRecorder()
		h.Accept(w, req)
		if w.Code != http.StatusNoContent {
			t.Fatalf("accept %s invite: %d %s", item.Scope, w.Code, w.Body.String())
		}
	}

	teamInvite := create("team", &teamID)
	accept(teamInvite)
	var teamMember, leader bool
	if err := db.QueryRow(ctx, `SELECT true,leader FROM team_members WHERE team_id=$1 AND user_clerk_id='invitee-user'`, teamID).Scan(&teamMember, &leader); err != nil {
		t.Fatal(err)
	}
	if !teamMember || leader {
		t.Fatalf("team invite membership: member=%v leader=%v", teamMember, leader)
	}
	projectInvite := create("project", &projectID)
	accept(projectInvite)
	var projectMember bool
	if err := db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM project_members WHERE project_id=$1 AND user_clerk_id='invitee-user')`, projectID).Scan(&projectMember); err != nil {
		t.Fatal(err)
	}
	if !projectMember {
		t.Fatal("project invite did not create project membership")
	}
}

func TestInvitationCreationRejectsAPIKeys(t *testing.T) {
	h := System{admins: organizations.Admins{}}
	req := httptest.NewRequest(http.MethodPost, "/api/organizations/invitations", strings.NewReader(`{}`))
	req = req.WithContext(auth.WithAPIKey(req.Context(), "user", "org", "agent", "ai"))
	w := httptest.NewRecorder()
	h.Create(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("API key create status=%d, want 403", w.Code)
	}
	list := httptest.NewRequest(http.MethodGet, "/api/invitations", nil).WithContext(auth.WithAPIKey(context.Background(), "user", "org", "agent", "ai"))
	w = httptest.NewRecorder()
	h.Mine(w, list)
	if w.Code != http.StatusForbidden {
		t.Fatalf("API key list status=%d, want 403", w.Code)
	}
}
