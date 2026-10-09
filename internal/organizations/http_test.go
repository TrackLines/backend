package organizations

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/clerk/clerk-sdk-go/v2/organizationmembership"
	"github.com/tracklines/backend/internal/auth"
)

type membershipListerFunc func(context.Context, *organizationmembership.ListParams) (*clerk.OrganizationMembershipList, error)

func (f membershipListerFunc) List(ctx context.Context, params *organizationmembership.ListParams) (*clerk.OrganizationMembershipList, error) {
	return f(ctx, params)
}

func TestMembersListsCurrentOrganizationWithoutRoles(t *testing.T) {
	first, last, image := "Alex", "Example", "https://images.example/alex.png"
	var gotOrg string
	var gotLimit, gotOffset int64
	h := System{memberships: membershipListerFunc(func(_ context.Context, params *organizationmembership.ListParams) (*clerk.OrganizationMembershipList, error) {
		gotOrg = params.OrganizationID
		gotLimit = *params.Limit
		gotOffset = *params.Offset
		return &clerk.OrganizationMembershipList{
			TotalCount: 1,
			OrganizationMemberships: []*clerk.OrganizationMembership{{
				Role: "org:admin",
				PublicUserData: &clerk.OrganizationMembershipPublicUserData{
					UserID: "user_1", FirstName: &first, LastName: &last, ImageURL: &image, Identifier: "alex@example.com",
				},
			}},
		}, nil
	})}
	ctx := auth.WithAPIKey(t.Context(), "agent-owner", "org_1", "codex", "ai")
	req := httptest.NewRequest("GET", "/api/organizations/members", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	h.Members(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	if gotOrg != "org_1" || gotLimit != defaultLimit || gotOffset != 0 {
		t.Fatalf("membership query org=%q limit=%d offset=%d", gotOrg, gotLimit, gotOffset)
	}
	if strings.Contains(rec.Body.String(), "role") {
		t.Fatalf("response exposes Clerk role: %s", rec.Body)
	}
	var got MemberPage
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.TotalCount != 1 || len(got.Members) != 1 || got.Members[0].UserID != "user_1" || got.Members[0].Identifier != "alex@example.com" {
		t.Fatalf("members page: %+v", got)
	}
}

func TestMembersPaginationAndMissingOrganization(t *testing.T) {
	calls := 0
	h := System{memberships: membershipListerFunc(func(context.Context, *organizationmembership.ListParams) (*clerk.OrganizationMembershipList, error) {
		calls++
		return &clerk.OrganizationMembershipList{}, nil
	})}
	for _, test := range []struct {
		url  string
		ctx  context.Context
		want int
	}{
		{url: "/api/organizations/members?limit=0", ctx: auth.WithAPIKey(t.Context(), "u", "org_1", "agent", "ai"), want: 400},
		{url: "/api/organizations/members?offset=-1", ctx: auth.WithAPIKey(t.Context(), "u", "org_1", "agent", "ai"), want: 400},
		{url: "/api/organizations/members", ctx: auth.WithAPIKey(t.Context(), "u", "", "agent", "ai"), want: 403},
	} {
		req := httptest.NewRequest("GET", test.url, nil).WithContext(test.ctx)
		rec := httptest.NewRecorder()
		h.Members(rec, req)
		if rec.Code != test.want {
			t.Errorf("%s: status = %d, want %d", test.url, rec.Code, test.want)
		}
	}
	if calls != 0 {
		t.Fatalf("membership client called %d times for invalid requests", calls)
	}
}

func TestMembersClerkFailure(t *testing.T) {
	h := System{memberships: membershipListerFunc(func(context.Context, *organizationmembership.ListParams) (*clerk.OrganizationMembershipList, error) {
		return nil, errors.New("clerk unavailable")
	})}
	ctx := auth.WithAPIKey(t.Context(), "u", "org_1", "agent", "ai")
	req := httptest.NewRequest("GET", "/api/organizations/members", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	h.Members(rec, req)
	if rec.Code != 502 {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
}
