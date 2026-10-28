package clerkcache

import (
	"context"
	"errors"
	"net"
	"net/url"
	"syscall"
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
	// TL-520: a membership change forgets that org's lists, and only that org's
	m.Forget(ctx, "org_a")
	m.List(ctx, params("org_a"))
	m.List(ctx, params("org_b"))
	if calls != 3 {
		t.Fatalf("after forgetting org_a: %d calls, want 3 (org_a refetched, org_b still cached)", calls)
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
	(Memberships{}).Forget(ctx, "org_d") // nothing to forget without Valkey
}

// flaky fails its first calls with err, then answers.
type flaky struct {
	calls *int
	fails int
	err   error
}

func (f flaky) List(ctx context.Context, p *organizationmembership.ListParams) (*clerk.OrganizationMembershipList, error) {
	if *f.calls++; *f.calls <= f.fails {
		return nil, f.err
	}
	return countingClerk{new(int)}.List(ctx, p)
}

// TL-710: a dropped connection to Clerk is tried once more; Clerk's own errors aren't.
func TestMembershipsRetryDroppedConnection(t *testing.T) {
	ctx := context.Background()
	reset := &url.Error{Op: "Get", URL: "https://api.clerk.com", Err: &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}}
	params := &organizationmembership.ListParams{OrganizationID: "org_a"}

	calls := 0
	if _, err := (Memberships{Next: flaky{&calls, 1, reset}}).List(ctx, params); err != nil || calls != 2 {
		t.Fatalf("reset once: %v after %d calls, want an answer after 2", err, calls)
	}
	calls = 0
	if _, err := (Memberships{Next: flaky{&calls, 2, reset}}).List(ctx, params); !errors.Is(err, syscall.ECONNRESET) || calls != 2 {
		t.Fatalf("reset twice: %v after %d calls, want the error after 2", err, calls)
	}
	calls = 0
	if _, err := (Memberships{Next: flaky{&calls, 1, errors.New("forbidden")}}).List(ctx, params); err == nil || calls != 1 {
		t.Fatalf("clerk error: %v after %d calls, want no retry", err, calls)
	}
	calls = 0
	gone, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := (Memberships{Next: flaky{&calls, 1, reset}}).List(gone, params); err == nil || calls != 1 {
		t.Fatalf("caller gone: %v after %d calls, want no retry", err, calls)
	}
}
