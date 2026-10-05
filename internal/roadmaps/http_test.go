package roadmaps

import (
	"net/http"
	"testing"
)

func TestCanRead(t *testing.T) {
	cases := []struct {
		vis, owner, user string
		signedIn         bool
		want             int
	}{
		{Public, "o", "", false, 0},
		{LoginOnly, "o", "", false, http.StatusUnauthorized},
		{LoginOnly, "o", "x", true, 0},
		{Team, "o", "", false, http.StatusUnauthorized},
		{Team, "o", "x", true, http.StatusForbidden},
		{Team, "o", "o", true, 0},
		{Team, "", "", false, http.StatusUnauthorized}, // empty owner must not match anon
	}
	for _, c := range cases {
		if got := canRead(&Roadmap{Visibility: c.vis, OwnerClerkID: c.owner}, c.user, c.signedIn); got != c.want {
			t.Errorf("%+v: got %d", c, got)
		}
	}
}
