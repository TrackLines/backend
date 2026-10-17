package invitations

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/clerk/clerk-sdk-go/v2/organizationmembership"
	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/organizations"
)

// countedMembers is Clerk's member list for an organization of a given size.
type countedMembers struct{ total int64 }

func (c countedMembers) List(_ context.Context, p *organizationmembership.ListParams) (*clerk.OrganizationMembershipList, error) {
	if len(p.EmailAddresses) > 0 { // "is this person already a member?": no
		return &clerk.OrganizationMembershipList{}, nil
	}
	return &clerk.OrganizationMembershipList{TotalCount: c.total}, nil
}

// TL-418: Free and Pro organizations can't invite past 20 members, counting invitations not yet
// accepted; Enterprise can.
func TestInvitationsStopAtTheMemberLimit(t *testing.T) {
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
	if _, err := db.Exec(ctx, `DELETE FROM invitations WHERE org_id IN ('lim-free', 'lim-ent'); DELETE FROM org_admins WHERE org_id IN ('lim-free', 'lim-ent'); DELETE FROM org_billing WHERE org_id IN ('lim-free', 'lim-ent');
		INSERT INTO org_admins (org_id, user_clerk_id, granted_by) VALUES ('lim-free', 'lim-admin', 'test'), ('lim-ent', 'lim-admin', 'test');
		INSERT INTO org_billing (org_id, stripe_status, plan) VALUES ('lim-ent', 'active', 'enterprise');
		INSERT INTO invitations (org_id, clerk_invitation_id, email_address, scope, invited_by, accept_url, expires_at) VALUES
			('lim-free', 'orginv_pending', 'waiting@example.test', 'organization', 'lim-admin', 'https://x', now() + interval '3 days'),
			('lim-free', 'orginv_expired', 'late@example.test', 'organization', 'lim-admin', 'https://x', now() - interval '1 day')`); err != nil {
		t.Fatal(err)
	}
	invite := func(org string, members int64) *httptest.ResponseRecorder {
		h := NewSystem(db, organizations.Admins{DB: db})
		h.invitations = &fakeInvites{next: int(time.Now().UnixNano() % 1e9), invitation: &clerk.OrganizationInvitation{URL: "https://clerk.example/invite", ExpiresAt: clerk.Int64(time.Now().Add(7 * 24 * time.Hour).UnixMilli())}}
		h.memberships = countedMembers{members}
		h.users = fakeUsers{}
		c := clerk.ContextWithSessionClaims(ctx, func() *clerk.SessionClaims {
			s := &clerk.SessionClaims{RegisteredClaims: clerk.RegisteredClaims{Subject: "lim-admin"}}
			s.ActiveOrganizationID = org
			return s
		}())
		body, _ := json.Marshal(inviteInput{EmailAddress: "new@example.test", Scope: "organization"})
		req := httptest.NewRequest(http.MethodPost, "/api/organizations/invitations", strings.NewReader(string(body))).WithContext(c)
		rec := httptest.NewRecorder()
		h.Create(rec, req)
		db.Exec(ctx, `DELETE FROM invitations WHERE email_address = 'new@example.test'`)
		return rec
	}
	// 18 members + 1 pending invitation (the expired one doesn't count) = 19: one more fits
	if rec := invite("lim-free", 18); rec.Code != http.StatusCreated {
		t.Errorf("free, 19 of 20: %d %s", rec.Code, rec.Body)
	}
	// 19 + 1 pending = 20: full
	rec := invite("lim-free", 19)
	if rec.Code != http.StatusPaymentRequired || !strings.Contains(rec.Body.String(), "up to 20 members") || !strings.Contains(rec.Body.String(), "Enterprise") {
		t.Errorf("free, 20 of 20: %d %s", rec.Code, rec.Body)
	}
	// Enterprise: no limit
	if rec := invite("lim-ent", 250); rec.Code != http.StatusCreated {
		t.Errorf("enterprise, 250 members: %d %s", rec.Code, rec.Body)
	}
}
