package tickets

import (
	"context"
	"testing"

	"github.com/clerk/clerk-sdk-go/v2"
	"github.com/clerk/clerk-sdk-go/v2/organizationmembership"
	"github.com/clerk/clerk-sdk-go/v2/user"
	"github.com/tracklines/backend/internal/notifications"
)

type namedMembers []clerk.OrganizationMembershipPublicUserData

func (m namedMembers) List(context.Context, *organizationmembership.ListParams) (*clerk.OrganizationMembershipList, error) {
	list := &clerk.OrganizationMembershipList{TotalCount: int64(len(m))}
	for i := range m {
		list.OrganizationMemberships = append(list.OrganizationMemberships, &clerk.OrganizationMembership{PublicUserData: &m[i]})
	}
	return list, nil
}

type namedUsers map[string]string // user id → username

func (u namedUsers) List(_ context.Context, params *user.ListParams) (*clerk.UserList, error) {
	list := &clerk.UserList{}
	for _, id := range params.UserIDs {
		if name, ok := u[id]; ok {
			list.Users = append(list.Users, &clerk.User{ID: id, Username: &name})
		}
	}
	return list, nil
}

// The @ picker gets each person's real name (even "You") and username; @username notifies.
// Clerk's membership list leaves usernames empty (TL-187), so they come from its user list.
func TestMentionPeople(t *testing.T) {
	s := func(v string) *string { return &v }
	h := System{memberships: namedMembers{
		{UserID: "user_alan", FirstName: s("Alan"), LastName: s("Smith")},
		{UserID: "user_me", FirstName: s("Ava"), LastName: s("Jones")},
	}, users: namedUsers{"user_alan": "bob"}}
	got, err := h.organizationAssignees(context.Background(), "org", "user_me")
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Name != "Alan Smith" || got[0].Username != "bob" || got[1].Label != "You" || got[1].Name != "Ava Jones" || got[1].Username != "" {
		t.Fatalf("assignees: %+v", got)
	}
	people, err := h.People(context.Background(), "org")
	if err != nil {
		t.Fatal(err)
	}
	if to := notifications.Mentioned("thanks @bob", people); len(to) != 1 || to[0] != "user_alan" {
		t.Fatalf("@username: %v", to)
	}
}
