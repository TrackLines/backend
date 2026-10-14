package tickets

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/clerk/clerk-sdk-go/v2/organizationmembership"
	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tracklines/backend/internal/auth"
)

// orgMembers is Clerk's membership list for specific orgs: org -> its members.
type orgMembers map[string][]string

func (m orgMembers) List(_ context.Context, p *organizationmembership.ListParams) (*clerk.OrganizationMembershipList, error) {
	list := &clerk.OrganizationMembershipList{}
	for _, id := range m[p.OrganizationID] {
		if len(p.UserIDs) == 0 || slices.Contains(p.UserIDs, id) {
			list.OrganizationMemberships = append(list.OrganizationMemberships, &clerk.OrganizationMembership{PublicUserData: &clerk.OrganizationMembershipPublicUserData{UserID: id}})
		}
	}
	list.TotalCount = int64(len(list.OrganizationMemberships))
	return list, nil
}

// A ticket link opened with another of your orgs active can say which org it's in (TL-276); to
// someone outside that org, or an API key, the ticket doesn't exist.
func TestWhereIsTicket(t *testing.T) {
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
	if _, err := db.Exec(ctx, `DELETE FROM projects WHERE owner_clerk_id = 'org_wh'`); err != nil {
		t.Fatal(err)
	}
	var pid string
	_ = db.QueryRow(ctx, `INSERT INTO projects (owner_clerk_id, name) VALUES ('org_wh', 'Where') RETURNING id`).Scan(&pid)
	tk, err := Store{DB: db}.CreateBacklog(ctx, "org_wh", pid, "task", "over here", "")
	if err != nil {
		t.Fatal(err)
	}
	h := System{store: Store{DB: db}, memberships: orgMembers{"org_wh": {"user_both"}, "org_other": {"user_both", "user_out"}}}
	where := func(c context.Context, id string) (int, string) {
		req := httptest.NewRequest("GET", "/", nil).WithContext(c)
		req.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		h.Where(rec, req)
		return rec.Code, rec.Body.String()
	}
	session := func(user, active string) context.Context {
		c := &clerk.SessionClaims{RegisteredClaims: clerk.RegisteredClaims{Subject: user}}
		c.ActiveOrganizationID = active
		return clerk.ContextWithSessionClaims(ctx, c)
	}
	// a member of both, with the other org active: told which org it's in
	if code, body := where(session("user_both", "org_other"), tk.ID); code != 200 || !strings.Contains(body, `"org_id":"org_wh"`) {
		t.Fatalf("member of its org: %d %s", code, body)
	}
	// not a member of its org, an API key, or no such ticket: it doesn't exist
	for name, c := range map[string]struct {
		ctx context.Context
		id  string
	}{
		"outsider":  {session("user_out", "org_other"), tk.ID},
		"API key":   {auth.WithAPIKey(ctx, "user_both", "org_other", "agent", "ai"), tk.ID},
		"no ticket": {session("user_both", "org_other"), "00000000-0000-0000-0000-000000000000"},
		"not an id": {session("user_both", "org_other"), "TL-1"},
	} {
		if code, body := where(c.ctx, c.id); code != 404 {
			t.Errorf("%s: %d %s", name, code, body)
		}
	}
}
