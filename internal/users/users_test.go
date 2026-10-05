package users

import (
	"testing"

	"github.com/clerk/clerk-sdk-go/v2"
)

func TestPrimaryEmail(t *testing.T) {
	p := "e2"
	u := &clerk.User{PrimaryEmailAddressID: &p, EmailAddresses: []*clerk.EmailAddress{{ID: "e1", EmailAddress: "a@x"}, {ID: "e2", EmailAddress: "b@x"}}}
	if got := primaryEmail(u); got != "b@x" {
		t.Fatalf("got %q", got)
	}
	if got := primaryEmail(&clerk.User{}); got != "" {
		t.Fatalf("no emails: got %q", got)
	}
}
