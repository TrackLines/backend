package apikeys

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tracklines/backend/internal/auth"
)

// TL-167: bf_ keys are retired. Only tl_ keys are API keys; a bf_ token is never looked up or
// treated as one (so a bf_ row could never grant access), and a signed-in route refuses it.
func TestBfKeysAreNotKeys(t *testing.T) {
	var sawKey bool
	h := Middleware(nil)(auth.Required(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawKey = auth.ViaAPIKey(r.Context())
		w.WriteHeader(http.StatusNoContent)
	})))
	req := httptest.NewRequest("GET", "/api/projects", nil)
	req.Header.Set("Authorization", "Bearer bf_anything")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req) // a nil database would panic if the bf_ token were looked up
	if rec.Code != http.StatusUnauthorized || sawKey {
		t.Fatalf("bf_ token: %d (key %v)", rec.Code, sawKey)
	}
}
