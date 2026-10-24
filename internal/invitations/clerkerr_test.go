package invitations

import (
	"context"
	"net/http/httptest"
	"net/url"
	"testing"
)

// A browser that left mid-request isn't an error to report: Clerk's client wraps the cancel in a *url.Error.
func TestClerkErrSkipsCanceled(t *testing.T) {
	rec := httptest.NewRecorder()
	writeClerkErr(rec, httptest.NewRequest("GET", "/", nil), "load invitation recipient", &url.Error{Op: "Get", URL: "https://api.clerk.com/v1/users/x", Err: context.Canceled})
	if rec.Body.Len() != 0 {
		t.Fatalf("cancelled request answered: %d %s", rec.Code, rec.Body)
	}
}
