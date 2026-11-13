package billing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/clerk/clerk-sdk-go/v2/organization"
	"github.com/clerk/clerk-sdk-go/v2/organizationmembership"
	"github.com/tracklines/backend/internal/notifications"
)

type clerkOrgList []*clerk.Organization

func (f clerkOrgList) List(_ context.Context, p *organization.ListParams) (*clerk.OrganizationList, error) {
	start := int(*p.Offset)
	end := min(start+int(*p.Limit), len(f))
	if start > len(f) {
		start = end
	}
	return &clerk.OrganizationList{Organizations: f[start:end], TotalCount: int64(len(f))}, nil
}

type fakeMembers map[string][]string

func (f fakeMembers) List(_ context.Context, p *organizationmembership.ListParams) (*clerk.OrganizationMembershipList, error) {
	var out []*clerk.OrganizationMembership
	for _, u := range f[p.OrganizationID] {
		out = append(out, &clerk.OrganizationMembership{PublicUserData: &clerk.OrganizationMembershipPublicUserData{UserID: u}})
	}
	return &clerk.OrganizationMembershipList{OrganizationMemberships: out, TotalCount: int64(len(out))}, nil
}

type pushed struct {
	to     [][]string
	urgent []bool
}

func (p *pushed) Notify(_ context.Context, _ string, to []string, e notifications.Event) error {
	p.to = append(p.to, to)
	p.urgent = append(p.urgent, e.Urgent)
	return nil
}

// TL-778: staff see every org's offer status, and send the offer to the ones they pick, once each.
func TestLaunchOfferStaff(t *testing.T) {
	ctx := context.Background()
	db := launchDB(t)
	if _, err := db.Exec(ctx, `DELETE FROM launch_offers WHERE org_id LIKE 'org_st%';
		DELETE FROM org_billing WHERE org_id LIKE 'org_st%';
		DELETE FROM notifications WHERE org_id LIKE 'org_st%';
		DELETE FROM projects WHERE owner_clerk_id LIKE 'org_st%';
		INSERT INTO projects (owner_clerk_id, name, updated_at) VALUES ('org_stpaid', 'p', '2026-11-10T09:00:00Z');
		INSERT INTO org_billing (org_id, stripe_subscription_id, stripe_status, plan) VALUES
			('org_stpaid', 'sub_p', 'active', 'pro'), ('org_stclaimed', 'sub_c', 'trialing', 'pro'),
			('org_stconv', 'sub_v', 'active', 'pro'), ('org_stended', 'sub_e', 'canceled', 'pro');
		INSERT INTO launch_offers (org_id, offered_by, claimed_at, claimed_by, subscription_id, trial_ends_at) VALUES
			('org_stsent', 'staff', NULL, NULL, NULL, NULL),
			('org_stclaimed', 'staff', now(), 'user_c', 'sub_c', now() + interval '300 days'),
			('org_stconv', 'staff', now(), 'user_v', 'sub_v', now() - interval '1 day'),
			('org_stended', 'staff', now(), 'user_e', 'sub_e', now() - interval '1 day')`); err != nil {
		t.Fatal(err)
	}
	day := int64(24 * time.Hour / time.Millisecond)
	now := time.Now().UnixMilli()
	two := int64(2)
	orgs := clerkOrgList{
		{ID: "org_stnew", Name: "New Co", CreatedAt: now - day, MembersCount: &two},
		{ID: "org_stpaid", Name: "Paying", CreatedAt: now - 2*day},
		{ID: "org_stsent", Name: "Sent", CreatedAt: now - 3*day},
		{ID: "org_stclaimed", Name: "Claimed", CreatedAt: now - 4*day},
		{ID: "org_stconv", Name: "Converted", CreatedAt: now - 5*day},
		{ID: "org_stended", Name: "Ended", CreatedAt: now - 6*day},
	}
	on := false
	push := &pushed{}
	s := LaunchOfferStaff{DB: db, On: func() bool { return on }, Orgs: orgs,
		Members: fakeMembers{"org_stnew": {"user_boss", "user_dev"}}, IsAdmin: adminsExcept("user_dev"), Push: push,
		Urgent: func(_ context.Context, typ string) bool { return typ == "offer" }} // policy2's answer today (TL-882)

	list := func() (map[string]OfferOrg, []string, bool) {
		rec := httptest.NewRecorder()
		s.List(rec, httptest.NewRequest("GET", "/", nil))
		var body struct {
			Organizations []OfferOrg `json:"organizations"`
			FlagOn        bool       `json:"flag_on"`
		}
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &body) != nil {
			t.Fatalf("list: %d %s", rec.Code, rec.Body)
		}
		byID, order := map[string]OfferOrg{}, []string{}
		for _, o := range body.Organizations {
			byID[o.OrgID], order = o, append(order, o.OrgID)
		}
		return byID, order, body.FlagOn
	}
	got, order, flag := list()
	if flag {
		t.Fatal("flag reported on")
	}
	if !slices.Equal(order, []string{"org_stnew", "org_stpaid", "org_stsent", "org_stclaimed", "org_stconv", "org_stended"}) {
		t.Fatalf("newest first: %v", order)
	}
	for id, want := range map[string]string{"org_stnew": OfferNotSent, "org_stpaid": OfferPaid, "org_stsent": OfferSent,
		"org_stclaimed": OfferClaimed, "org_stconv": OfferConverted, "org_stended": OfferEnded} {
		if got[id].Status != want {
			t.Errorf("%s: %s, want %s", id, got[id].Status, want)
		}
	}
	if c := got["org_stclaimed"]; c.ClaimedBy == nil || *c.ClaimedBy != "user_c" || c.TrialEndsAt == nil || c.Plan != Pro {
		t.Errorf("claimed row: %+v", c)
	}
	if a := got["org_stpaid"].LastActive; a == nil || !a.Equal(time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("last active: %v", a)
	}
	if got["org_stnew"].LastActive != nil {
		t.Error("org with no projects has a last active date")
	}
	if n := got["org_stnew"]; n.Name != "New Co" || n.Members != 2 || n.Plan != Free || n.CreatedAt.IsZero() {
		t.Errorf("new org row: %+v", n)
	}

	send := func(body string) (int, map[string]json.RawMessage) {
		rec := httptest.NewRecorder()
		s.Send(rec, signedIn(httptest.NewRequest("POST", "/", strings.NewReader(body)), "user_staff"))
		var out map[string]json.RawMessage
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	// flag off: nobody is told about an offer they can't claim
	if code, _ := send(`{"org_ids":["org_stnew"]}`); code != http.StatusConflict {
		t.Fatalf("flag off send: %d", code)
	}
	on = true
	if code, _ := send(`{"org_ids":[]}`); code != http.StatusBadRequest {
		t.Fatalf("empty send: %d", code)
	}
	code, out := send(`{"org_ids":["org_stnew","org_stpaid","org_stsent","org_stnew"]}`)
	if code != 200 || string(out["sent"]) != `["org_stnew"]` ||
		string(out["skipped"]) != `[{"org_id":"org_stpaid","reason":"paid"},{"org_id":"org_stsent","reason":"already_sent"}]` {
		t.Fatalf("send: %d %s %s", code, out["sent"], out["skipped"])
	}
	var by string
	if err := db.QueryRow(ctx, `SELECT offered_by FROM launch_offers WHERE org_id = 'org_stnew'`).Scan(&by); err != nil || by != "user_staff" {
		t.Fatalf("offer recorded: %q %v", by, err)
	}
	texts := map[string]string{}
	rows, err := db.Query(ctx, `SELECT user_id, text FROM notifications WHERE org_id = 'org_stnew' AND kind = 'billing' AND link = $1 AND urgent`, LaunchOfferLink)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var u, text string
		_ = rows.Scan(&u, &text)
		texts[u] = text
	}
	if texts["user_boss"] != offerTextAdmin || texts["user_dev"] != offerTextMember {
		t.Fatalf("notices: %v", texts)
	}
	if len(push.to) != 2 || !slices.Equal(push.to[0], []string{"user_boss"}) || !slices.Equal(push.to[1], []string{"user_dev"}) || !slices.Equal(push.urgent, []bool{true, true}) {
		t.Fatalf("push: %v", push.to)
	}
	// once only: sending again tells nobody twice
	if code, out := send(`{"org_ids":["org_stnew"]}`); code != 200 || string(out["sent"]) != `[]` {
		t.Fatalf("resend: %d %s", code, out["sent"])
	}
	var n int
	_ = db.QueryRow(ctx, `SELECT count(*) FROM notifications WHERE org_id = 'org_stnew'`).Scan(&n)
	if n != 2 {
		t.Fatalf("notices after resend: %d", n)
	}
	if got, _, flag := list(); got["org_stnew"].Status != OfferSent || got["org_stnew"].OfferedAt == nil || !flag {
		t.Fatalf("after send: %+v %v", got["org_stnew"], flag)
	}
}
