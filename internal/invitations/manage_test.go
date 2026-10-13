package invitations

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/clerk/clerk-sdk-go/v2/organizationinvitation"
	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/organizations"
)

// failingRevoke is Clerk refusing to revoke.
type failingRevoke struct{ *fakeInvites }

func (failingRevoke) Revoke(context.Context, *organizationinvitation.RevokeParams) (*clerk.OrganizationInvitation, error) {
	return nil, errors.New("clerk is down")
}

// Admins list, read and revoke their org's invitations; members and other orgs can't.
func TestInvitationListGetRevoke(t *testing.T) {
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
	const org, other = "org_iv1", "org_iv2"
	for _, q := range []string{
		`DELETE FROM invitations WHERE org_id IN ($1, $2)`, `DELETE FROM org_admins WHERE org_id IN ($1, $2)`,
		`INSERT INTO org_admins (org_id, user_clerk_id, granted_by) VALUES ($1, 'admin-u', 'test'), ($2, 'admin-x', 'test')`,
	} {
		if _, err := db.Exec(ctx, q, org, other); err != nil {
			t.Fatal(err)
		}
	}
	invites := &fakeInvites{next: 9000, invitation: &clerk.OrganizationInvitation{URL: "https://clerk.example/invite", ExpiresAt: clerk.Int64(time.Now().Add(7 * 24 * time.Hour).UnixMilli())}}
	h := NewSystem(db, organizations.Admins{DB: db})
	h.invitations, h.memberships, h.users = invites, fakeMemberships{}, fakeUsers{}
	as := func(o, user string) context.Context {
		c := &clerk.SessionClaims{RegisteredClaims: clerk.RegisteredClaims{Subject: user}}
		c.ActiveOrganizationID = o
		return clerk.ContextWithSessionClaims(ctx, c)
	}
	run := func(action, o, user, id, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest("POST", "/", strings.NewReader(body)).WithContext(as(o, user))
		req.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		map[string]func(){"create": func() { h.Create(rec, req) }, "list": func() { h.List(rec, req) },
			"get": func() { h.Get(rec, req) }, "revoke": func() { h.Revoke(rec, req) }}[action]()
		return rec
	}
	expect := func(want int, action, o, user, id, body string, out any) {
		t.Helper()
		rec := run(action, o, user, id, body)
		if rec.Code != want {
			t.Fatalf("%s as %s/%s %s: %d %s, want %d", action, o, user, id, rec.Code, rec.Body, want)
		}
		if out != nil && json.Unmarshal(rec.Body.Bytes(), out) != nil {
			t.Fatalf("%s: bad JSON %s", action, rec.Body)
		}
	}
	var ava, bo Invitation
	expect(201, "create", org, "admin-u", "", `{"email_address":"ava@example.test","scope":"organization"}`, &ava)
	expect(201, "create", org, "admin-u", "", `{"email_address":"bo@example.test","scope":"organization"}`, &bo)

	// list: admins of the org only; an expired pending invitation reads as expired
	if _, err := db.Exec(ctx, `UPDATE invitations SET expires_at = now() - interval '1 day' WHERE id = $1`, bo.ID); err != nil {
		t.Fatal(err)
	}
	var list []Invitation
	expect(200, "list", org, "admin-u", "", "", &list)
	status := map[string]string{}
	for _, i := range list {
		status[i.EmailAddress] = i.Status
	}
	if len(list) != 2 || status["ava@example.test"] != "pending" || status["bo@example.test"] != "expired" {
		t.Fatalf("list: %+v", list)
	}
	expect(403, "list", org, "member-u", "", "", nil)
	expect(200, "list", other, "admin-x", "", "", &list)
	if len(list) != 0 {
		t.Fatalf("another org's list: %+v", list)
	}

	// get: admins of the org only
	var got Invitation
	expect(200, "get", org, "admin-u", ava.ID, "", &got)
	if got.ID != ava.ID || got.EmailAddress != "ava@example.test" {
		t.Fatalf("get: %+v", got)
	}
	expect(200, "get", org, "admin-u", bo.ID, "", &got)
	if got.Status != "expired" {
		t.Fatalf("expired get: %+v", got)
	}
	expect(403, "get", org, "member-u", ava.ID, "", nil)
	expect(404, "get", other, "admin-x", ava.ID, "", nil)
	expect(404, "get", org, "admin-u", "not-an-id", "", nil)

	// revoke: Clerk failing leaves it pending (502); then revoked in Clerk and here; only pending ones
	h.invitations = failingRevoke{invites}
	expect(502, "revoke", org, "admin-u", ava.ID, "", nil)
	expect(200, "get", org, "admin-u", ava.ID, "", &got)
	if got.Status != "pending" {
		t.Fatalf("after a failed revoke: %s", got.Status)
	}
	h.invitations = invites
	expect(403, "revoke", org, "member-u", ava.ID, "", nil)
	expect(404, "revoke", other, "admin-x", ava.ID, "", nil)
	expect(204, "revoke", org, "admin-u", ava.ID, "", nil)
	expect(200, "get", org, "admin-u", ava.ID, "", &got)
	if got.Status != "revoked" || invites.invitation.Status != "revoked" {
		t.Fatalf("revoked: here %s, Clerk %s", got.Status, invites.invitation.Status)
	}
	expect(409, "revoke", org, "admin-u", ava.ID, "", nil)
}
