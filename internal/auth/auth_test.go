package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/clerk/clerk-sdk-go/v2"
)

func TestRequired(t *testing.T) {
	h := Required(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	for _, hdr := range []string{"", "Bearer not-a-jwt"} {
		req := httptest.NewRequest("GET", "/", nil)
		if hdr != "" {
			req.Header.Set("Authorization", hdr)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%q: got %d, want 401", hdr, rec.Code)
		}
	}
	opt := Optional(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := UserID(r.Context()); ok {
			t.Fatal("anon request has a user id")
		}
	}))
	rec := httptest.NewRecorder()
	opt.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("optional anon: got %d", rec.Code)
	}
}

func TestAPIKeyKind(t *testing.T) {
	ctx := WithAPIKeyUser(t.Context(), "owner", "deploy", "service")
	if !ViaAPIKey(ctx) || APIKeyKind(ctx) != "service" || ActorID(ctx) != "deploy" {
		t.Fatalf("service key context: via=%v kind=%q actor=%q", ViaAPIKey(ctx), APIKeyKind(ctx), ActorID(ctx))
	}
	legacy := WithAPIKeyUser(t.Context(), "owner", "codex")
	if APIKeyKind(legacy) != "ai" {
		t.Fatalf("legacy key kind = %q, want ai", APIKeyKind(legacy))
	}
}

func TestOrgID(t *testing.T) {
	session := func(c *clerk.SessionClaims) context.Context {
		return clerk.ContextWithSessionClaims(context.Background(), c)
	}
	v2 := &orgClaims{}
	v2.O.ID = "org_v2"
	cases := map[string]struct {
		ctx  context.Context
		want string
	}{
		"v2 token (o.id)":     {session(&clerk.SessionClaims{RegisteredClaims: clerk.RegisteredClaims{Subject: "u"}, Custom: v2}), "org_v2"},
		"v1 token (org_id)":   {session(&clerk.SessionClaims{RegisteredClaims: clerk.RegisteredClaims{Subject: "u"}, Claims: clerk.Claims{ActiveOrganizationID: "org_v1"}}), "org_v1"},
		"no active org":       {session(&clerk.SessionClaims{RegisteredClaims: clerk.RegisteredClaims{Subject: "u"}}), ""},
		"api key acts in org": {WithAPIKey(context.Background(), "u", "org_k", "codex", "ai"), "org_k"},
		"pre-org api key":     {WithAPIKey(context.Background(), "u", "", "codex", "ai"), ""},
	}
	for name, c := range cases {
		if got := OrgID(c.ctx); got != c.want {
			t.Errorf("%s: got %q, want %q", name, got, c.want)
		}
	}

	// Required: signed in without an org is 403, with one is let through
	ok := Required(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	for ctx, want := range map[context.Context]int{cases["no active org"].ctx: http.StatusForbidden, cases["v2 token (o.id)"].ctx: http.StatusOK} {
		rec := httptest.NewRecorder()
		ok.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil).WithContext(ctx))
		if rec.Code != want {
			t.Fatalf("required: got %d, want %d", rec.Code, want)
		}
	}
}
