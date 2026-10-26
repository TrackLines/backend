package staff

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/clerk/clerk-sdk-go/v2"

	"github.com/tracklines/backend/internal/auth"
)

// Only members of the staff org get in, whatever org they have active; everyone else gets 404 (TL-573).
func TestOnlyStaff(t *testing.T) {
	member := func(_ context.Context, org, user string) (bool, error) {
		if user == "broken" {
			return false, errors.New("clerk down")
		}
		return org == "org_cf" && user == "staff", nil
	}
	session := func(user string) context.Context {
		return clerk.ContextWithSessionClaims(context.Background(), &clerk.SessionClaims{RegisteredClaims: clerk.RegisteredClaims{Subject: user}})
	}
	on := func() bool { return true }
	cases := []struct {
		name string
		g    Guard
		ctx  context.Context
		want int
	}{
		{"staff", Guard{"org_cf", member, on}, session("staff"), 200},
		{"not staff", Guard{"org_cf", member, on}, session("someone"), 404},
		{"signed out", Guard{"org_cf", member, on}, context.Background(), 404},
		{"staff's API key", Guard{"org_cf", member, on}, auth.WithAPIKey(context.Background(), "staff", "org_cf", "k", "ai"), 404},
		{"no staff org", Guard{"", member, on}, session("staff"), 404},
		{"flag off", Guard{"org_cf", member, func() bool { return false }}, session("staff"), 404},
		{"clerk down", Guard{"org_cf", member, on}, session("broken"), 503},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		c.g.Only(http.HandlerFunc(Me)).ServeHTTP(rec, httptest.NewRequest("GET", "/api/staff/me", nil).WithContext(c.ctx))
		if rec.Code != c.want {
			t.Errorf("%s: %d, want %d", c.name, rec.Code, c.want)
		}
		if c.want == 200 && rec.Body.String() != "{\"staff\":true}\n" {
			t.Errorf("%s: body %q", c.name, rec.Body.String())
		}
	}
}
