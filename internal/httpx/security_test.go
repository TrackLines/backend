package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSecurityHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	SecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "nope", http.StatusNotFound) })).
		ServeHTTP(rec, httptest.NewRequest("GET", "/api/x", nil))
	for k, v := range map[string]string{
		"X-Content-Type-Options": "nosniff", "X-Frame-Options": "DENY", "Content-Security-Policy": "frame-ancestors 'none'",
		"Referrer-Policy": "no-referrer", "Strict-Transport-Security": "max-age=31536000; includeSubDomains",
	} {
		if got := rec.Header().Get(k); got != v {
			t.Errorf("%s = %q, want %q (on errors too)", k, got, v)
		}
	}
}
