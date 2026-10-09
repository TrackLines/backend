package clerkcache

import (
	"context"
	"testing"
	"time"

	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/clerk/clerk-sdk-go/v2/organizationmembership"
	tcvalkey "github.com/testcontainers/testcontainers-go/modules/valkey"
	"github.com/valkey-io/valkey-go"
)

type countingClerk struct{ calls *int }

func (c countingClerk) List(_ context.Context, p *organizationmembership.ListParams) (*clerk.OrganizationMembershipList, error) {
	*c.calls++
	return &clerk.OrganizationMembershipList{TotalCount: 1, OrganizationMemberships: []*clerk.OrganizationMembership{
		{PublicUserData: &clerk.OrganizationMembershipPublicUserData{UserID: "user_" + p.OrganizationID}},
	}}, nil
}

func TestMemberships(t *testing.T) {
	ctx := context.Background()
	container, err := tcvalkey.Run(ctx, "valkey/valkey:8-alpine")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = container.Terminate(ctx) })
	url, err := container.ConnectionString(ctx)
	if err != nil {
		t.Fatal(err)
	}
	opt, _ := valkey.ParseURL(url)
	vk, err := valkey.NewClient(opt)
	if err != nil {
		t.Fatal(err)
	}
	defer vk.Close()

	calls := 0
	m := Memberships{Next: countingClerk{&calls}, VK: vk, TTL: time.Minute}
	params := func(org string) *organizationmembership.ListParams {
		limit := int64(500)
		return &organizationmembership.ListParams{OrganizationID: org, ListParams: clerk.ListParams{Limit: &limit}}
	}
	for range 3 {
		list, err := m.List(ctx, params("org_a"))
		if err != nil || list.OrganizationMemberships[0].PublicUserData.UserID != "user_org_a" {
			t.Fatalf("list: %+v %v", list, err)
		}
	}
	if calls != 1 {
		t.Fatalf("clerk called %d times, want 1 (then cached)", calls)
	}
	// a different org (or page) is a different entry
	if list, _ := m.List(ctx, params("org_b")); list.OrganizationMemberships[0].PublicUserData.UserID != "user_org_b" || calls != 2 {
		t.Fatalf("org_b: %d calls", calls)
	}
	// Valkey gone: still answers, straight from Clerk
	vk.Close()
	if list, err := m.List(ctx, params("org_c")); err != nil || list.OrganizationMemberships[0].PublicUserData.UserID != "user_org_c" {
		t.Fatalf("valkey down: %+v %v", list, err)
	}
	// no Valkey configured at all
	if _, err := (Memberships{Next: countingClerk{&calls}}).List(ctx, params("org_d")); err != nil {
		t.Fatal(err)
	}
}
