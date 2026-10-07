package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
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
