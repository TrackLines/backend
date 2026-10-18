package tickets

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tracklines/backend/internal/auth"
)

func TestServiceKeyCannotClaimOrReleaseTickets(t *testing.T) {
	h := System{}
	ctx := auth.WithAPIKeyUser(context.Background(), "owner", "deploy", "service")

	claim := httptest.NewRecorder()
	h.Claim(claim, httptest.NewRequest(http.MethodPost, "/tickets/t1/claim", nil).WithContext(ctx))
	if claim.Code != http.StatusForbidden {
		t.Fatalf("claim status = %d, want %d", claim.Code, http.StatusForbidden)
	}

	release := httptest.NewRecorder()
	h.Release(release, httptest.NewRequest(http.MethodPost, "/tickets/t1/release", nil).WithContext(ctx))
	if release.Code != http.StatusForbidden {
		t.Fatalf("release status = %d, want %d", release.Code, http.StatusForbidden)
	}
}

// a personal key is its owner (TL-450): it gets past the service-key gate (and on to the store)
func TestPersonalKeyMayClaim(t *testing.T) {
	ctx := auth.WithAPIKey(context.Background(), "owner", "org", "", "personal")
	defer func() { _ = recover() }() // System{} has no store: reaching it is the point
	rec := httptest.NewRecorder()
	System{}.Claim(rec, httptest.NewRequest(http.MethodPost, "/tickets/t1/claim", nil).WithContext(ctx))
	if rec.Code == http.StatusForbidden {
		t.Fatalf("personal key refused: %s", rec.Body)
	}
}
