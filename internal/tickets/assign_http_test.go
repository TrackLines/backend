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
