package billing

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/clerk/clerk-sdk-go/v2/organization"
	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5/pgxpool"
)

// fakeOrgs records the member cap Clerk is told for each organization.
type fakeOrgs map[string]int64

func (f fakeOrgs) Update(_ context.Context, id string, p *organization.UpdateParams) (*clerk.Organization, error) {
	f[id] = *p.MaxAllowedMemberships
	return &clerk.Organization{ID: id}, nil
}

// TL-418: Clerk's member cap follows the plan: 20 on Free and Pro, unlimited (0) on Enterprise.
func TestMemberCaps(t *testing.T) {
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
	if _, err := db.Exec(ctx, `DELETE FROM org_billing WHERE org_id LIKE 'org_mc%'; DELETE FROM projects WHERE owner_clerk_id LIKE 'org_mc%'; DELETE FROM member_caps WHERE org_id LIKE 'org_mc%';
		INSERT INTO org_billing (org_id, stripe_status, plan) VALUES ('org_mc_pro', 'active', 'pro'), ('org_mc_ent', 'active', 'enterprise'), ('org_mc_lapsed', 'canceled', 'enterprise');
		INSERT INTO projects (owner_clerk_id, name) VALUES ('org_mc_free', 'p')`); err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{"org_mc_free": 20, "org_mc_pro": 20, "org_mc_ent": 0, "org_mc_lapsed": 20} // a lapsed plan is Free
	got := fakeOrgs{}
	SyncAllMemberCaps(ctx, db, got)
	for org, cap := range want {
		if v, ok := got[org]; !ok || v != cap {
			t.Errorf("%s: cap %d (set %v), want %d", org, v, ok, cap)
		}
	}
	// TL-427: the next start only tells Clerk about caps that changed
	again := fakeOrgs{}
	SyncAllMemberCaps(ctx, db, again)
	if len(again) != 0 {
		t.Errorf("unchanged caps sent to Clerk again: %v", again)
	}
	db.Exec(ctx, `UPDATE org_billing SET stripe_status = 'active' WHERE org_id = 'org_mc_lapsed'`)
	SyncAllMemberCaps(ctx, db, again)
	if len(again) != 1 || again["org_mc_lapsed"] != 0 {
		t.Errorf("after org_mc_lapsed renewed Enterprise: %v, want only it, at 0", again)
	}
	for org, limit := range map[string]int{"org_mc_free": 20, "org_mc_pro": 20, "org_mc_ent": -1} {
		if l, err := MemberLimitOf(ctx, db, org); err != nil || l != limit {
			t.Errorf("%s: member limit %d %v, want %d", org, l, err, limit)
		}
	}
	// an organization in CLERK_UNCAPPED_ORGS is left alone
	skipped := fakeOrgs{}
	if err := SyncMemberCap(ctx, db, Skip{OrgUpdater: skipped, Orgs: []string{"org_mc_ent"}}, "org_mc_ent"); err != nil || len(skipped) != 0 {
		t.Errorf("uncapped org: Clerk told %v (%v)", skipped, err)
	}
	// upgrading lifts it; nil Clerk client does nothing
	db.Exec(ctx, `UPDATE org_billing SET plan = 'enterprise' WHERE org_id = 'org_mc_pro'`)
	if err := SyncMemberCap(ctx, db, got, "org_mc_pro"); err != nil || got["org_mc_pro"] != 0 {
		t.Errorf("upgraded to Enterprise: %d %v", got["org_mc_pro"], err)
	}
	if err := SyncMemberCap(ctx, db, nil, "org_mc_pro"); err != nil {
		t.Error(err)
	}
}
